package stocklib

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTP 客户端（东财/腾讯/新浪统一超时）
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
}

func doGet(fullURL string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ============ 东方财富 clist 列表接口 ============

const EM_URL = "http://push2delay.eastmoney.com/api/qt/clist/get"

var EM_HEADERS = map[string]string{
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36",
}

type EMClistResp struct {
	Data struct {
		Total int                      `json:"total"`
		Diff  []map[string]interface{} `json:"diff"`
	} `json:"data"`
}

// FetchEM 请求东财 clist，params 含 pn/pz/po/np/fltt/invt/fid/fs/fields
// 默认参数与原版一致：pn=1 po=1 np=1 fltt=2 invt=2
func FetchEM(params map[string]string) (*EMClistResp, error) {
	if _, ok := params["pn"]; !ok {
		params["pn"] = "1"
	}
	if _, ok := params["po"]; !ok {
		params["po"] = "1"
	}
	if _, ok := params["np"]; !ok {
		params["np"] = "1"
	}
	if _, ok := params["fltt"]; !ok {
		params["fltt"] = "2"
	}
	if _, ok := params["invt"]; !ok {
		params["invt"] = "2"
	}
	uv := url.Values{}
	for k, v := range params {
		uv.Set(k, v)
	}
	body, err := doGet(EM_URL+"?"+uv.Encode(), EM_HEADERS)
	if err != nil {
		return nil, err
	}
	var r EMClistResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ============ 东方财富 datacenter 数据接口 ============

const DATACENTER_URL = "https://datacenter-web.eastmoney.com/api/data/v1/get"

var FIN_HEADERS = map[string]string{
	"User-Agent": "Mozilla/5.0",
	"Referer":    "https://quote.eastmoney.com/",
}

// FetchDatacenter 请求 datacenter-web，返回成功行数组（原版 success+result.data）
func FetchDatacenter(params map[string]interface{}) ([]map[string]interface{}, error) {
	uv := url.Values{}
	for k, v := range params {
		uv.Set(k, fmt.Sprintf("%v", v))
	}
	body, err := doGet(DATACENTER_URL+"?"+uv.Encode(), FIN_HEADERS)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Success bool `json:"success"`
		Result  *struct {
			Data []map[string]interface{} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if !resp.Success || resp.Result == nil {
		return nil, nil
	}
	return resp.Result.Data, nil
}

// ============ 腾讯日K接口 (qfq) ============

// TencentKline 拉取腾讯前复权日K。
// symbol 需带 sh/sz 前缀；返回日期、收盘价序列。
func TencentKline(symbol string, count int) ([]string, []float64, error) {
	u := fmt.Sprintf("https://web.ifzq.gtimg.cn/appstock/app/fqkline/get?param=%s,day,,,%d,qfq", symbol, count)
	body, err := doGet(u, map[string]string{"User-Agent": "Mozilla/5.0"})
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		Data map[string]struct {
			QFQDay [][]interface{} `json:"qfqday"`
			Day    [][]interface{} `json:"day"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}
	node, ok := resp.Data[symbol]
	if !ok {
		return nil, nil, fmt.Errorf("接口无该股票数据")
	}
	rows := node.QFQDay
	if len(rows) == 0 {
		rows = node.Day
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("无K线数据")
	}
	var dates []string
	var closes []float64
	for _, item := range rows {
		if len(item) < 3 {
			continue
		}
		date, ok1 := item[0].(string)
		closeStr, ok2 := item[2].(string)
		if !ok1 || !ok2 {
			continue
		}
		var c float64
		if _, err := fmt.Sscanf(closeStr, "%g", &c); err != nil {
			continue
		}
		dates = append(dates, date)
		closes = append(closes, c)
	}
	if len(dates) == 0 {
		return nil, nil, fmt.Errorf("K线数据为空")
	}
	return dates, closes, nil
}

// ============ 新浪K线接口（app_v9 的 HMM 位置用） ============

type SinaKlineItem struct {
	Close string `json:"close"`
	Day   string `json:"day"`
}

// SinaKline 拉取新浪日K（scale=240 即日线）, 返回收盘价序列。
func SinaKline(symbol string, datalen int) ([]float64, error) {
	u := fmt.Sprintf("http://money.finance.sina.com.cn/quotes_service/api/json_v2.php/CN_MarketData.getKLineData?symbol=%s&scale=240&ma=no&datalen=%d", symbol, datalen)
	body, err := doGet(u, map[string]string{"Referer": "https://finance.sina.com.cn"})
	if err != nil {
		return nil, err
	}
	var items []SinaKlineItem
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	var closes []float64
	for _, it := range items {
		var c float64
		if _, err := fmt.Sscanf(it.Close, "%g", &c); err != nil {
			continue
		}
		closes = append(closes, c)
	}
	return closes, nil
}

// MarketSymbol 转换纯代码为带市场前缀的行情代码：6开头→sh，0/3开头→sz（与 app_v9 market_symbol 一致）
func MarketSymbol(code string) string {
	if strings.HasPrefix(code, "6") {
		return "sh" + code
	}
	if strings.HasPrefix(code, "0") || strings.HasPrefix(code, "3") {
		return "sz" + code
	}
	return "sh" + code
}

// TencentSymbol 腾讯K线前缀规则：60/68开头→sh，其余→sz（与 app_v9_multi get_hmm_position 一致）
func TencentSymbol(code string) string {
	if strings.HasPrefix(code, "60") || strings.HasPrefix(code, "68") {
		return "sh" + code
	}
	return "sz" + code
}