package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
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

// ============ 常量（与原版 app_v9_multi.py 完全一致） ============

const (
	CACHE_DIR       = "cache"
	PORT            = 18088
	SCAN_INTERVAL   = 1800 // 30分钟一轮
	MAIN_BOARD_FS   = "m:0+t:6,m:1+t:2" // 沪深主板
	BATCH_SIZE      = 200
	FIN_BATCH_SIZE  = 400
	FCAST_BATCH_SIZE = 450
	MAX_WORKERS     = 8
)

type MainStock struct {
	Code              string   `json:"code"`
	Name              string   `json:"name"`
	Price             float64  `json:"price"`
	Change            float64  `json:"change"`
	HmmPos            int      `json:"hmm_pos"`
	FlowRatio         *float64 `json:"flow_ratio"`
	NetFlowPriceRatio *float64 `json:"net_flow_price_ratio"`
	Sector            string   `json:"sector"`
	Roe               *float64 `json:"roe"`
	Growth            *float64 `json:"growth"`
	ForecastType      *string  `json:"forecast_type"`
	ForecastDate      *string  `json:"forecast_date"`
}

type MainBoardCache struct {
	CachedAt     string      `json:"cached_at"`
	ElapsedSec   float64     `json:"elapsed_sec"`
	TotalScanned int         `json:"total_scanned"`
	Stocks       []MainStock `json:"stocks"`
}

var (
	scanMu    sync.Mutex
	scanning  bool
	cacheLock sync.RWMutex
)

func init() {
	os.MkdirAll(CACHE_DIR, 0755)
}

func mainBoardCachePath() string {
	return filepath.Join(CACHE_DIR, "main_board_prefilter.json")
}

func isTradingDay(now time.Time) bool {
	// 周一~周五 True（与原版 now.weekday() < 5 一致）
	return now.Weekday() >= time.Monday && now.Weekday() <= time.Friday
}

// ============ 全市场沪深主板扫描 ============

// fetch_main_board: 东财 clist 翻页拉全量沪深主板
// 返回 []row{code,name,price,change,f62,f20,sector}
func fetchMainBoard() [][]interface{} {
	var rows [][]interface{}
	for pn := 1; pn <= 100; pn++ {
		data, err := stocklib.FetchEM(map[string]string{
			"pn": strconv.Itoa(pn), "pz": "100", "fid": "f12",
			"fs": MAIN_BOARD_FS, "fields": "f12,f14,f2,f3,f62,f20,f100",
		})
		if err != nil {
			log.Printf("[main] 主板列表第%d页获取失败: %v", pn, err)
			break
		}
		diff := data.Data.Diff
		if len(diff) == 0 {
			break
		}
		for _, it := range diff {
			code, _ := it["f12"].(string)
			code = strings.TrimSpace(code)
			for len(code) < 6 {
				code = "0" + code
			}
			if code == "" {
				continue
			}
			name, _ := it["f14"].(string)
			rows = append(rows, []interface{}{code, name, it["f2"], it["f3"], it["f62"], it["f20"], it["f100"]})
		}
	}
	return rows
}

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
	case nil:
		return 0, false
	}
	return 0, false
}

func toFloatPtr(v interface{}) *float64 {
	f, ok := toFloat(v)
	if !ok {
		return nil
	}
	return &f
}

func toStrPtr(v interface{}) *string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

func round2(x float64) float64 {
	return float64(int(x*100+0.5)) / 100
}

// _worker 计算单只股票 (与原版 scan_all_main_board._worker 一致)
func worker(row []interface{}) *MainStock {
	code := row[0].(string)
	name := row[1].(string)
	price, _ := toFloat(row[2])
	if price <= 0 {
		return nil // 停牌/无价格跳过
	}
	change, _ := toFloat(row[3])
	if !okFloat(row[3]) {
		change = 0.0
	}

	// 净流占比
	var flowRatio *float64
	flow, hasFlow := toFloat(row[4])
	cap, hasCap := toFloat(row[5])
	if hasFlow && hasCap && cap > 0 {
		fr := round2(flow / cap * 100)
		flowRatio = &fr
	}

	// 净流涨跌同比
	var netFlowPriceRatio *float64
	if flowRatio != nil && *flowRatio != 0 {
		nfr := round2((change - *flowRatio) / *flowRatio * 100)
		netFlowPriceRatio = &nfr
	}

	hmmPos := stocklib.HMMPositionTencent(code, price)
	if hmmPos == -1 {
		time.Sleep(200 * time.Millisecond)
		hmmPos = stocklib.HMMPositionTencent(code, price)
		if hmmPos == -1 {
			log.Printf("[scan] 重试失败 %s", code)
		}
	}

	sector, _ := row[6].(string)
	return &MainStock{
		Code: code, Name: name, Price: round2(price), Change: round2(change),
		HmmPos: hmmPos, FlowRatio: flowRatio, NetFlowPriceRatio: netFlowPriceRatio,
		Sector: sector,
	}
}

func okFloat(v interface{}) bool {
	switch x := v.(type) {
	case float64:
		return true
	case json.Number:
		return true
	case string:
		return x != ""
	}
	return false
}

func fetchFinRoe(codes []string) map[string]struct {
	Roe    *float64 `json:"roe"`
	Growth *float64 `json:"growth"`
} {
	result := map[string]struct {
		Roe    *float64 `json:"roe"`
		Growth *float64 `json:"growth"`
	}{}
	for i := 0; i < len(codes); i += FIN_BATCH_SIZE {
		end := i + FIN_BATCH_SIZE
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[i:end]
		quoted := make([]string, len(batch))
		for j, c := range batch {
			quoted[j] = `"` + c + `"`
		}
		codeFilter := "(" + strings.Join(quoted, ",") + ")"
		rows, err := stocklib.FetchDatacenter(map[string]interface{}{
			"sortColumns": "REPORTDATE", "sortTypes": -1,
			"pageSize": 500, "pageNumber": 1,
			"reportName": "RPT_LICO_FN_CPD",
			"columns":    "SECURITY_CODE,REPORTDATE,WEIGHTAVG_ROE,SJLTZ",
			"filter":     "(SECURITY_CODE in " + codeFilter + `)(REPORTDATE>='2026-03-31')`,
		})
		if err != nil {
			log.Printf("[fin] 财务批次[%d] 异常: %v", i/FIN_BATCH_SIZE+1, err)
		} else if len(rows) == 0 {
			log.Printf("[fin] 财务批次[%d] %d只→0行", i/FIN_BATCH_SIZE+1, len(batch))
		} else {
			for _, item := range rows {
				code, _ := item["SECURITY_CODE"].(string)
				if code == "" {
					continue
				}
				if _, exists := result[code]; !exists {
					result[code] = struct {
						Roe    *float64 `json:"roe"`
						Growth *float64 `json:"growth"`
					}{toFloatPtr(item["WEIGHTAVG_ROE"]), toFloatPtr(item["SJLTZ"])}
				}
			}
			log.Printf("[fin] 财务批次[%d] %d只→%d行", i/FIN_BATCH_SIZE+1, len(batch), len(rows))
		}
		if end < len(codes) {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return result
}

func fetchForecast(codes []string) map[string]struct {
	Type *string `json:"type"`
	Date *string `json:"date"`
} {
	result := map[string]struct {
		Type *string `json:"type"`
		Date *string `json:"date"`
	}{}
	for i := 0; i < len(codes); i += FCAST_BATCH_SIZE {
		end := i + FCAST_BATCH_SIZE
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[i:end]
		quoted := make([]string, len(batch))
		for j, c := range batch {
			quoted[j] = `"` + c + `"`
		}
		codeFilter := "(" + strings.Join(quoted, ",") + ")"
		rows, err := stocklib.FetchDatacenter(map[string]interface{}{
			"sortColumns": "NOTICE_DATE", "sortTypes": -1,
			"pageSize": 500, "pageNumber": 1,
			"reportName": "RPT_PUBLIC_OP_NEWPREDICT",
			"columns":    "SECURITY_CODE,NOTICE_DATE,PREDICT_TYPE,IS_LATEST",
			"filter":     "(SECURITY_CODE in " + codeFilter + `)(REPORT_DATE>='2025-12-31')(IS_LATEST="T")`,
		})
		if err != nil {
			log.Printf("[fcast] 预告批次[%d] 异常: %v", i/FCAST_BATCH_SIZE+1, err)
		} else if len(rows) == 0 {
			log.Printf("[fcast] 预告批次[%d] %d只→0行", i/FCAST_BATCH_SIZE+1, len(batch))
		} else {
			for _, item := range rows {
				code, _ := item["SECURITY_CODE"].(string)
				if code == "" {
					continue
				}
				if _, exists := result[code]; !exists {
					result[code] = struct {
						Type *string `json:"type"`
						Date *string `json:"date"`
					}{toStrPtr(item["PREDICT_TYPE"]), toStrPtr(item["NOTICE_DATE"])}
				}
			}
			log.Printf("[fcast] 预告批次[%d] %d只→%d行", i/FCAST_BATCH_SIZE+1, len(batch), len(rows))
		}
		if end < len(codes) {
			time.Sleep(300 * time.Millisecond)
		}
	}
	return result
}

// scanAllMainBoard 全市场并发扫描并写缓存
func scanAllMainBoard() *MainBoardCache {
	board := fetchMainBoard()
	total := len(board)
	if total == 0 {
		log.Printf("[main] 主板列表为空")
		payload := &MainBoardCache{
			CachedAt: time.Now().Format("2006-01-02 15:04:05"),
			ElapsedSec: 0, TotalScanned: 0, Stocks: []MainStock{},
		}
		stocklib.CacheWriteJSON(mainBoardCachePath(), payload)
		return payload
	}

	start := time.Now()
	var results []*MainStock

	// 每批 BATCH_SIZE 只，8 并发，批次间 sleep 0.2s
	for i := 0; i < len(board); i += BATCH_SIZE {
		end := i + BATCH_SIZE
		if end > len(board) {
			end = len(board)
		}
		batch := board[i:end]
		jobs := make(chan []interface{}, len(batch))
		out := make(chan *MainStock, len(batch))
		var wg sync.WaitGroup
		for w := 0; w < MAX_WORKERS; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for row := range jobs {
					out <- worker(row)
				}
			}()
		}
		for _, row := range batch {
			jobs <- row
		}
		close(jobs)
		wg.Wait()
		close(out)
		for r := range out {
			if r != nil {
				results = append(results, r)
			}
		}
		if end < len(board) {
			time.Sleep(200 * time.Millisecond)
		}
	}

	// 收集命中
	var hits []MainStock
	for _, s := range results {
		if s.HmmPos == 1 || s.HmmPos == 2 {
			hits = append(hits, *s)
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Code < hits[j].Code })

	// 批量财务 + 预告
	if len(hits) > 0 {
		hitCodes := make([]string, len(hits))
		for i, s := range hits {
			hitCodes[i] = s.Code
		}
		finMap := fetchFinRoe(hitCodes)
		fcastMap := fetchForecast(hitCodes)
		for i := range hits {
			if fin, ok := finMap[hits[i].Code]; ok {
				hits[i].Roe = fin.Roe
				hits[i].Growth = fin.Growth
			}
			if fc, ok := fcastMap[hits[i].Code]; ok {
				hits[i].ForecastType = fc.Type
				hits[i].ForecastDate = fc.Date
			}
		}
	}

	elapsed := time.Since(start).Seconds()
	payload := &MainBoardCache{
		CachedAt: time.Now().Format("2006-01-02 15:04:05"),
		ElapsedSec: math.Round(elapsed*10) / 10, TotalScanned: total, Stocks: hits,
	}
	stocklib.CacheWriteJSON(mainBoardCachePath(), payload)

	great, ok := 0, 0
	nRoe, nGrowth, nFc, nFlow, nSector, nNfpr := 0, 0, 0, 0, 0, 0
	for _, s := range hits {
		if s.HmmPos == 1 {
			great++
		}
		if s.HmmPos == 2 {
			ok++
		}
		if s.Roe != nil {
			nRoe++
		}
		if s.Growth != nil {
			nGrowth++
		}
		if s.ForecastType != nil {
			nFc++
		}
		if s.FlowRatio != nil {
			nFlow++
		}
		if s.Sector != "" {
			nSector++
		}
		if s.NetFlowPriceRatio != nil {
			nNfpr++
		}
	}
	log.Printf("[main] 全市场扫描完成: %d 只(👍%d/👌%d), 扫描%d只, 耗时 %.1fs", len(hits), great, ok, total, payload.ElapsedSec)
	log.Printf("[main] 字段覆盖 roe=%d growth=%d forecast=%d flow_ratio=%d sector=%d net_flow_price_ratio=%d", nRoe, nGrowth, nFc, nFlow, nSector, nNfpr)
	return payload
}

func refreshScan() error {
	if !scanMu.TryLock() {
		log.Printf("[main] 扫描已在运行, 本次跳过")
		return nil
	}
	defer scanMu.Unlock()
	setScanning(true)
	defer setScanning(false)
	scanAllMainBoard()
	return nil
}

func setScanning(v bool) {
	cacheLock.Lock()
	scanning = v
	cacheLock.Unlock()
}

func isScanning() bool {
	cacheLock.RLock()
	defer cacheLock.RUnlock()
	return scanning
}

func readMainCache() *MainBoardCache {
	var payload MainBoardCache
	if err := stocklib.CacheReadJSON(mainBoardCachePath(), &payload); err != nil {
		return nil
	}
	return &payload
}

func backgroundScanner() {
	for {
		if !isTradingDay(time.Now()) {
			log.Printf("[bg] 非交易日，沿用缓存 (SCAN_INTERVAL=%ds)", SCAN_INTERVAL)
		} else {
			refreshScan()
		}
		time.Sleep(SCAN_INTERVAL * time.Second)
	}
}

// ============ HTTP 处理 ============

func apiMulti(w http.ResponseWriter, r *http.Request) {
	fresh := r.URL.Query().Get("fresh")
	isFresh := fresh == "1" || fresh == "true" || fresh == "True"

	payload := readMainCache()
	if payload == nil {
		// 缓存不存在 → 异步触发后台扫描
		go refreshScan()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stocks": []MainStock{}, "count": 0, "cached_at": "",
			"total_scanned": 0, "scanning": true,
			"message": "全市场首轮扫描进行中，请稍后刷新",
		})
		return
	}
	if isFresh {
		go refreshScan()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"stocks": payload.Stocks, "count": len(payload.Stocks),
		"cached_at":    payload.CachedAt,
		"total_scanned": payload.TotalScanned,
		"elapsed_sec":  payload.ElapsedSec,
		"scanning":     isScanning(),
	})
}

// apiChart 代理转发到 5800（复刻原版代理到 5000）
func apiChart(w http.ResponseWriter, req *http.Request) {
	code := strings.TrimPrefix(req.URL.Path, "/stock/api/chart/")
	target := "http://127.0.0.1:5800/stock/api/chart/" + code
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{"error": fmt.Sprintf("图表服务不可用: %v", err)})
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	// 透传 body
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
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

func refreshCache(w http.ResponseWriter, r *http.Request) {
	count := 0
	files, _ := os.ReadDir(CACHE_DIR)
	for _, f := range files {
		name := f.Name()
		if strings.HasSuffix(name, ".json") && name != "stocks_v9.json" {
			os.Remove(filepath.Join(CACHE_DIR, name))
			count++
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{"message": fmt.Sprintf("已清除 %d 个缓存文件", count)})
}

// ============ 主入口 ============

func main() {
	htmlPage, err := os.ReadFile("page.html")
	if err != nil {
		log.Fatalf("读取 page.html 失败: %v", err)
	}
	page := string(htmlPage)

	// 后台全市场扫描线程
	go backgroundScanner()

	mux := http.NewServeMux()

	// 静态资源
	mux.Handle("/stock/static/", http.StripPrefix("/stock/static/", http.FileServer(http.Dir("static"))))

	// 页面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/stock/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/multi/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})
	mux.HandleFunc("/multi", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page))
	})

	// 接口
	mux.HandleFunc("/stock/api/multi", apiMulti)
	mux.HandleFunc("/stock/api/chart/", apiChart)
	mux.HandleFunc("/api/scan-ports", scanPorts)
	mux.HandleFunc("/api/refresh-cache", refreshCache)

	log.Printf("多条件筛选 V9 (Go) 启动于 :%d", PORT)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", PORT), mux))
}