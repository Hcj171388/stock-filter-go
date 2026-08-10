package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"stocklib"
)

// ============ 常量（与原版 app_v9.py 完全一致） ============

const (
	CACHE_DIR         = "cache"
	STOCK_CACHE_TTL   = 600
	CHART_CACHE_TTL   = 900
	SECTOR_CACHE_TTL  = 300
	FINANCE_CACHE_TTL = 3600
	PORT              = 5800
)

// 过滤无效板块关键词
var EXCLUDE_SECTOR_KEYWORDS = []string{
	"风格", "指数", "组合", "涨停", "连板", "新高", "反转",
	"金股", "做市", "GDR", "ST", "百日", "茅", "宁",
	"地区", "区域", "西部", "东北", "中部", "高振", "振幅",
	"多板", "首板", "跌停", "异动", "放量", "缩量",
	"强势", "弱势", "反弹", "回调", "突破", "均线",
}

// 过滤垃圾股预告类型
var EXCLUDE_FORECAST_TYPES = []string{"续亏", "增亏", "首亏", "预减"}

type Stock struct {
	Code          string   `json:"code"`
	Name          string   `json:"name"`
	Price         float64  `json:"price"`
	Change        float64  `json:"change"`
	Flow          float64  `json:"flow"`
	FlowRatio     float64  `json:"flow_ratio"`
	MktCap        float64  `json:"mkt_cap"`
	Sector        string   `json:"sector"`
	Roe           *float64 `json:"roe"`
	Growth        *float64 `json:"growth"`
	ForecastType  *string  `json:"forecast_type"`
	ForecastDate  *string  `json:"forecast_date"`
	HmmPos        int      `json:"hmm_pos"`
}

type Sector struct {
	Code   string  `json:"code"`
	Name   string  `json:"name"`
	Change float64 `json:"change"`
	Flow   float64 `json:"flow"`
	Type   string  `json:"type"`
}

type StocksCache struct {
	Stocks  []Stock  `json:"stocks"`
	Sectors []Sector `json:"sectors"`
	T       float64  `json:"t"`
}

var (
	moduleMu sync.Mutex
)

func init() {
	os.MkdirAll(CACHE_DIR, 0755)
}

// ============ 板块 ============

// is_valid_sector
func isValidSector(name string) bool {
	for _, kw := range EXCLUDE_SECTOR_KEYWORDS {
		if strings.Contains(name, kw) {
			return false
		}
	}
	return true
}

// get_top_sectors(limit=5)
func getTopSectors(limit int) []Sector {
	cacheFile := filepath.Join(CACHE_DIR, "top_sectors_v9.json")
	var cached struct {
		Sectors []Sector `json:"sectors"`
		T       float64  `json:"t"`
	}
	if ok, _ := stocklib.CacheCheckAge(cacheFile, SECTOR_CACHE_TTL); ok {
		if err := stocklib.CacheReadJSON(cacheFile, &cached); err == nil && cached.Sectors != nil {
			return cached.Sectors
		}
	}

	sectors := map[string]Sector{}
	for _, fsType := range []string{"m:90+t:2", "m:90+t:3"} {
		// 按涨幅排序
		if data, err := stocklib.FetchEM(map[string]string{
			"pz": "100", "fid": "f3", "fs": fsType, "fields": "f12,f14,f3,f62",
		}); err == nil {
			for _, s := range data.Data.Diff {
				name, _ := s["f14"].(string)
				if !isValidSector(name) {
					continue
				}
				code, _ := s["f12"].(string)
				if _, exists := sectors[code]; !exists {
					change, _ := toFloat(s["f3"])
					flow, _ := toFloat(s["f62"])
					sectorType := "概念"
					if strings.Contains(fsType, "t:2") {
						sectorType = "行业"
					}
					sectors[code] = Sector{code, name, change, flow, sectorType}
				}
			}
		}
		// 按主力净流入排序
		if data, err := stocklib.FetchEM(map[string]string{
			"pz": "100", "fid": "f62", "fs": fsType, "fields": "f12,f14,f3,f62",
		}); err == nil {
			for _, s := range data.Data.Diff {
				name, _ := s["f14"].(string)
				if !isValidSector(name) {
					continue
				}
				code, _ := s["f12"].(string)
				if _, exists := sectors[code]; !exists {
					change, _ := toFloat(s["f3"])
					flow, _ := toFloat(s["f62"])
					sectorType := "概念"
					if strings.Contains(fsType, "t:2") {
						sectorType = "行业"
					}
					sectors[code] = Sector{code, name, change, flow, sectorType}
				}
			}
		}
	}

	// 取涨幅前limit + 流入前limit
	var sectorList []Sector
	for _, s := range sectors {
		sectorList = append(sectorList, s)
	}
	sort.Slice(sectorList, func(i, j int) bool { return sectorList[i].Change > sectorList[j].Change })
	byGain := sectorList
	if len(byGain) > limit {
		byGain = byGain[:limit]
	}
	var byFlow []Sector
	for _, s := range sectorList {
		if s.Flow > 0 {
			byFlow = append(byFlow, s)
		}
	}
	sort.Slice(byFlow, func(i, j int) bool { return byFlow[i].Flow > byFlow[j].Flow })
	if len(byFlow) > limit {
		byFlow = byFlow[:limit]
	}

	// 合并去重
	resultMap := map[string]Sector{}
	result := []Sector{}
	for _, s := range byGain {
		resultMap[s.Code] = s
		result = append(result, s)
	}
	for _, s := range byFlow {
		if _, exists := resultMap[s.Code]; !exists {
			result = append(result, s)
		}
	}

	// 写缓存
	stocklib.CacheWriteJSON(cacheFile, map[string]interface{}{"sectors": result, "t": float64(time.Now().Unix())})
	return result
}

// get_sector_stocks(sector_code)
func getSectorStocks(sectorCode string) []Stock {
	data, err := stocklib.FetchEM(map[string]string{
		"pz": "500", "fid": "f3", "fs": "b:" + sectorCode, "fields": "f12,f14,f2,f3,f62,f20",
	})
	if err != nil {
		log.Printf("板块成分股获取失败 %s: %v", sectorCode, err)
		return nil
	}
	var stocks []Stock
	for _, s := range data.Data.Diff {
		price, _ := toFloat(s["f2"])
		if price <= 0 {
			continue
		}
		flow, _ := toFloat(s["f62"])
		mktCap, _ := toFloat(s["f20"])
		flowRatio := 0.0
		if mktCap > 0 {
			flowRatio = flow / mktCap * 100
		}
		code, _ := s["f12"].(string)
		name, _ := s["f14"].(string)
		change, _ := toFloat(s["f3"])
		stocks = append(stocks, Stock{
			Code: code, Name: name, Price: price, Change: change,
			Flow: flow, FlowRatio: flowRatio, MktCap: mktCap,
		})
	}
	return stocks
}

// ============ 财务与预告（批量，带缓存） ============

// batch_get_finance
func batchGetFinance(stocks []Stock) {
	if len(stocks) == 0 {
		return
	}
	codeSet := map[string]bool{}
	for _, s := range stocks {
		codeSet[s.Code] = true
	}
	var codes []string
	for c := range codeSet {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	joined := strings.Join(codes, "|")
	cacheFile := stocklib.CacheFileName("fin_", joined)

	finMap := map[string]struct {
		Roe    *float64 `json:"roe"`
		Growth *float64 `json:"growth"`
	}{}

	if ok, _ := stocklib.CacheCheckAge(cacheFile, FINANCE_CACHE_TTL); ok {
		var cached struct {
			Data map[string]struct {
				Roe    *float64 `json:"roe"`
				Growth *float64 `json:"growth"`
			} `json:"data"`
			T float64 `json:"t"`
		}
		if err := stocklib.CacheReadJSON(cacheFile, &cached); err == nil {
			for i := range stocks {
				if v, exists := cached.Data[stocks[i].Code]; exists {
					stocks[i].Roe = v.Roe
					stocks[i].Growth = v.Growth
				}
			}
			return
		}
	}

	// 原版 filter: (SECURITY_CODE in ("code1","code2",...))
	quoted := make([]string, len(codes))
	for i, c := range codes {
		quoted[i] = `"` + c + `"`
	}
	codeFilter := "(" + strings.Join(quoted, ",") + ")"
	rows, err := stocklib.FetchDatacenter(map[string]interface{}{
		"sortColumns": "REPORTDATE", "sortTypes": -1, "pageSize": len(codes) * 3,
		"pageNumber": 1, "reportName": "RPT_LICO_FN_CPD",
		"columns": "SECURITY_CODE,REPORTDATE,WEIGHTAVG_ROE,SJLTZ",
		"filter":  "(SECURITY_CODE in " + codeFilter + ")",
	})
	if err != nil {
		log.Printf("财务数据批量获取失败: %v", err)
		return
	}

	for _, item := range rows {
		code, _ := item["SECURITY_CODE"].(string)
		if code == "" {
			continue
		}
		if _, exists := finMap[code]; !exists {
			roe, _ := toFloatPtr(item["WEIGHTAVG_ROE"])
			growth, _ := toFloatPtr(item["SJLTZ"])
			finMap[code] = struct {
				Roe    *float64 `json:"roe"`
				Growth *float64 `json:"growth"`
			}{roe, growth}
		}
	}
	for i := range stocks {
		if v, exists := finMap[stocks[i].Code]; exists {
			stocks[i].Roe = v.Roe
			stocks[i].Growth = v.Growth
		}
	}
	stocklib.CacheWriteJSON(cacheFile, map[string]interface{}{"data": finMap, "t": float64(time.Now().Unix())})
}

// batch_get_forecast
func batchGetForecast(stocks []Stock) {
	if len(stocks) == 0 {
		return
	}
	codeSet := map[string]bool{}
	for _, s := range stocks {
		codeSet[s.Code] = true
	}
	var codes []string
	for c := range codeSet {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	joined := strings.Join(codes, "|")
	cacheFile := stocklib.CacheFileName("fcast_", joined)

	if ok, _ := stocklib.CacheCheckAge(cacheFile, FINANCE_CACHE_TTL); ok {
		var cached struct {
			Data map[string]struct {
				Type *string `json:"type"`
				Date *string `json:"date"`
			} `json:"data"`
			T float64 `json:"t"`
		}
		if err := stocklib.CacheReadJSON(cacheFile, &cached); err == nil {
			for i := range stocks {
				if v, exists := cached.Data[stocks[i].Code]; exists {
					stocks[i].ForecastType = v.Type
					stocks[i].ForecastDate = v.Date
				}
			}
			return
		}
	}

	quoted := make([]string, len(codes))
	for i, c := range codes {
		quoted[i] = `"` + c + `"`
	}
	codeFilter := "(" + strings.Join(quoted, ",") + ")"
	rows, err := stocklib.FetchDatacenter(map[string]interface{}{
		"sortColumns": "NOTICE_DATE", "sortTypes": -1, "pageSize": len(codes) * 2,
		"pageNumber": 1, "reportName": "RPT_PUBLIC_OP_NEWPREDICT",
		"columns": "SECURITY_CODE,NOTICE_DATE,PREDICT_TYPE,INCREASE_JZ,PREDICT_FINANCE_CODE,IS_LATEST",
		"filter":  "(SECURITY_CODE in " + codeFilter + `)(PREDICT_FINANCE_CODE="005")(IS_LATEST="T")`,
	})
	if err != nil {
		log.Printf("业绩预告批量获取失败: %v", err)
		return
	}

	fcastMap := map[string]struct {
		Type *string `json:"type"`
		Date *string `json:"date"`
	}{}
	for _, item := range rows {
		code, _ := item["SECURITY_CODE"].(string)
		if code == "" {
			continue
		}
		if _, exists := fcastMap[code]; !exists {
			typ := strPtr(item["PREDICT_TYPE"])
			date := strPtr(item["NOTICE_DATE"])
			fcastMap[code] = struct {
				Type *string `json:"type"`
				Date *string `json:"date"`
			}{typ, date}
		}
	}
	for i := range stocks {
		if v, exists := fcastMap[stocks[i].Code]; exists {
			stocks[i].ForecastType = v.Type
			stocks[i].ForecastDate = v.Date
		}
	}
	stocklib.CacheWriteJSON(cacheFile, map[string]interface{}{"data": fcastMap, "t": float64(time.Now().Unix())})
}

// ============ 主流程 ============

// get_all_stocks 主流程：板块→成分股→财务→预告→过滤→排序
func getAllStocks(forceRefresh bool) ([]Stock, []Sector, string) {
	cacheFile := filepath.Join(CACHE_DIR, "stocks_v9.json")
	if !forceRefresh {
		var c StocksCache
		if err := stocklib.CacheReadJSON(cacheFile, &c); err == nil {
			cachedAt := ""
			if c.T > 0 {
				cachedAt = time.Unix(int64(c.T), 0).Format("2006-01-02 15:04:05")
			}
			return c.Stocks, c.Sectors, cachedAt
		}
	}

	sectors := getTopSectors(5)
	seen := map[string]bool{}
	var allStocks []Stock

	for _, sec := range sectors {
		secStocks := getSectorStocks(sec.Code)
		for _, s := range secStocks {
			if seen[s.Code] {
				continue
			}
			s.Sector = sec.Name
			seen[s.Code] = true
			allStocks = append(allStocks, s)
		}
	}

	// 批量获取财务和预告
	batchGetFinance(allStocks)
	batchGetForecast(allStocks)

	// 过滤垃圾股
	var qualityStocks []Stock
	for _, s := range allStocks {
		forecast := ""
		if s.ForecastType != nil {
			forecast = *s.ForecastType
		}
		if contains(EXCLUDE_FORECAST_TYPES, forecast) {
			continue
		}
		if s.Roe != nil && *s.Roe <= 0 {
			continue
		}
		if s.Growth != nil && *s.Growth <= 0 {
			continue
		}
		qualityStocks = append(qualityStocks, s)
	}

	// 获取 HMM 位置
	for i := range qualityStocks {
		qualityStocks[i].HmmPos = stocklib.HMMPositionSina(qualityStocks[i].Code)
	}

	// 按扣非净利同比降序
	sort.Slice(qualityStocks, func(i, j int) bool {
		gi, gj := -999999.0, -999999.0
		if qualityStocks[i].Growth != nil {
			gi = *qualityStocks[i].Growth
		}
		if qualityStocks[j].Growth != nil {
			gj = *qualityStocks[j].Growth
		}
		return gi > gj
	})

	// 写缓存
	stocklib.CacheWriteJSON(cacheFile, map[string]interface{}{
		"stocks": qualityStocks, "sectors": sectors, "t": float64(time.Now().Unix()),
	})

	return qualityStocks, sectors, time.Now().Format("2006-01-02 15:04:05")
}

// ============ HMM图表 ============

// get_chart_data 复刻 app_v9.get_chart_data
func getChartData(code string) map[string]interface{} {
	cacheFile := filepath.Join(CACHE_DIR, "v9_chart_"+code+".json")
	if ok, _ := stocklib.CacheCheckAge(cacheFile, CHART_CACHE_TTL); ok {
		var c map[string]interface{}
		if err := stocklib.CacheReadJSON(cacheFile, &c); err == nil {
			return c
		}
	}
	symbol := stocklib.MarketSymbol(code)
	dates, closes, err := stocklib.TencentKline(symbol, 60)
	if err != nil {
		log.Printf("[kline] %s 获取失败: %v", code, err)
		return map[string]interface{}{"error": "暂无K线数据"}
	}
	if len(closes) < 5 {
		return map[string]interface{}{"error": "暂无K线数据"}
	}

	states, returns, stateParams, trans := stocklib.HMMStates(closes)

	// returns 保留6位小数
	ret := make([]float64, len(returns))
	for i, r := range returns {
		ret[i] = float64(int(r*1e6+0.5)) / 1e6
	}
	result := map[string]interface{}{
		"code": code, "dates": dates, "prices": closes, "states": states,
		"returns": ret, "state_params": stateParams, "trans_matrix": trans,
	}
	result["t"] = float64(time.Now().Unix())
	stocklib.CacheWriteJSON(cacheFile, result)
	delete(result, "t")
	return result
}

// ============ HTTP 处理 ============

func apiStocks(w http.ResponseWriter, r *http.Request) {
	priceMax := 20.0
	if v := r.URL.Query().Get("priceMax"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			priceMax = p
		}
	}
	fresh := r.URL.Query().Get("fresh")
	forceRefresh := fresh == "1" || fresh == "true" || fresh == "True"

	stocks, sectors, cachedAt := getAllStocks(forceRefresh)

	var filtered []Stock
	for _, s := range stocks {
		if s.Price <= priceMax {
			filtered = append(filtered, s)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"stocks": filtered, "sectors": sectors, "count": len(filtered), "cached_at": cachedAt,
	})
}

func apiChart(w http.ResponseWriter, r *http.Request, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	result := getChartData(code)
	if _, hasErr := result["error"]; hasErr {
		w.WriteHeader(500)
	}
	json.NewEncoder(w).Encode(result)
}

func refreshCache(w http.ResponseWriter, r *http.Request) {
	files, _ := os.ReadDir(CACHE_DIR)
	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, "chart_") || strings.HasPrefix(name, "v9_chart_") ||
			strings.HasPrefix(name, "stocks_v9") || strings.HasPrefix(name, "top_sectors") ||
			strings.HasPrefix(name, "fin_") || strings.HasPrefix(name, "fcast_") {
			os.Remove(filepath.Join(CACHE_DIR, name))
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "message": "缓存已清理"})
}

func scanPorts(w http.ResponseWriter, r *http.Request) {
	type check struct {
		port   int
		name   string
		icon   string
		path   string
		scheme string
	}
	portChecks := []check{
		{5000, "股票筛选器", "📈", "/stock/", "http"},
		{5001, "多条件筛选", "🔍", "/multi/", "http"},
		{5800, "股票筛选器(Go)", "📈", ":5800/stock/", "http"},
		{18088, "多条件筛选(Go)", "🔍", ":18088/multi/", "http"},
		{8088, "QwenPaw", "🤖", ":8088/", "http"},
		{11439, "宝塔面板", "🛠️", ":11439/647bd5db", "https"},
		{42669, "CodeBuddy Code", "🤖", ":42669/", "http"},
	}
	var services []map[string]interface{}
	for _, c := range portChecks {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.port), 2*time.Second)
		if err == nil {
			conn.Close()
			services = append(services, map[string]interface{}{
				"name": c.name, "port": c.port, "icon": c.icon,
				"url": c.scheme + "://47.102.124.170" + c.path, "status": "running",
			})
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"services": services})
}

// ============ 工具函数 ============

func toFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func toFloatPtr(v interface{}) (*float64, bool) {
	if v == nil {
		return nil, false
	}
	f, ok := toFloat(v)
	if !ok {
		return nil, false
	}
	return &f, true
}

func strPtr(v interface{}) *string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return nil
		}
		return &s
	}
	return nil
}

func contains(arr []string, s string) bool {
	for _, a := range arr {
		if a == s {
			return true
		}
	}
	return false
}

// ============ 主入口 ============

func main() {
	mux := http.NewServeMux()

	htmlPage, err := os.ReadFile("page.html")
	if err != nil {
		log.Fatalf("读取 page.html 失败: %v", err)
	}
	page := string(htmlPage)

	// 静态资源（原版由 nginx alias 提供，此处由 Go 直接托管）
	mux.Handle("/stock/static/", http.StripPrefix("/stock/static/", http.FileServer(http.Dir("static"))))

	// 页面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/stock/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})

	// 接口
	mux.HandleFunc("/stock/api/stocks", apiStocks)
	mux.HandleFunc("/api/stocks", apiStocks)
	mux.HandleFunc("/stock/api/chart/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/stock/api/chart/")
		apiChart(w, r, code)
	})
	mux.HandleFunc("/api/chart/", func(w http.ResponseWriter, r *http.Request) {
		code := strings.TrimPrefix(r.URL.Path, "/api/chart/")
		apiChart(w, r, code)
	})
	mux.HandleFunc("/stock/api/refresh-cache", refreshCache)
	mux.HandleFunc("/api/scan-ports", scanPorts)

	log.Printf("实时优质股筛选 V9 (Go) 启动于 :%d", PORT)
	moduleMu.Lock() // 保留，避免空锁被 lint 忽略
	moduleMu.Unlock()
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", PORT), mux))
}