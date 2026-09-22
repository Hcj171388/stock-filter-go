package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ============ 10jqka DataQ specific_data（板块榜）============
// 鉴权：cookie 复用 stock-flow 服务（/opt/stock-filter-go/stock-flow/cookie.txt）
// 仅 mainForce 数据源通过鉴权。返回 data.data[] 每行：code(48:88xxxx), values[{idx,value}]

const (
	dataqURL        = "https://dataq.10jqka.com.cn/fetch-data-server/fetch/v1/specific_data"
	dataqCookieFile = "/opt/stock-filter-go/stock-flow/cookie.txt"
)

// dataqSectorRow 一条板块记录（一次请求取全 90 个 industry_l1 板块）
type dataqSectorRow struct {
	Code   string  // 48:88xxxx
	Name   string  // 行业名
	Change *float64 // 涨跌幅%（SNAPSHOT）
	NetIn  *float64 // 主力净流入（亿元，原始值/1e8）
}

// fetchDataqSectors 拉取全部 industry_l1 行业板块的 涨跌幅+主力净流入。
// 失败返回 (nil, err)。
func fetchDataqSectors() ([]dataqSectorRow, error) {
	cookie, err := os.ReadFile(dataqCookieFile)
	if err != nil {
		return nil, fmt.Errorf("read dataq cookie: %w", err)
	}
	cookie = []byte(strings.TrimSpace(string(cookie)))

	payload := `{
		"code_selectors": {"intersection": [{"type": "tag", "values": ["industry_l1"]}]},
		"indexes": [
			{"index_id": "security_name"},
			{"index_id": "price_change_ratio_pct"},
			{"index_id": "inr-main_capital_net_inflow-sum", "time_type": "DAY_1", "timestamp": "0", "attribute": {"win_size": 1}}
		],
		"page_info": {"page_begin": 0, "page_size": 500, "code_begin": 0},
		"sort": [{"idx": 1, "type": "DESC"}]
	}`
	req, err := http.NewRequest("POST", dataqURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Platform", "mobileweb")
	req.Header.Set("Source-id", "mainForce")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", "dataq.10jqka.com.cn")
	req.Header.Set("User-Agent", "okhttp/4.9.0")
	req.Header.Set("Cookie", string(cookie))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Data []struct {
				Code   string `json:"code"`
				Values []struct {
					Idx   int             `json:"idx"`
					Value json.RawMessage `json:"value"`
				} `json:"values"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode dataq: %w", err)
	}

	var rows []dataqSectorRow
	for _, item := range result.Data.Data {
		var r dataqSectorRow
		r.Code = item.Code
		for _, v := range item.Values {
			switch v.Idx {
			case 0:
				var name string
				json.Unmarshal(v.Value, &name)
				r.Name = name
			case 1:
				var val float64
				json.Unmarshal(v.Value, &val)
				r.Change = &val
			case 2:
				var val float64
				json.Unmarshal(v.Value, &val)
				yi := round2(val / 1e8)
				r.NetIn = &yi
			}
		}
		if r.Name != "" {
			rows = append(rows, r)
		}
	}
	return rows, nil
}

// ============ 扶摇（板块成分股）============
// 鉴权：env FUYAO_API_KEY（同花顺金融数据 API，x-api-key）
// 板块码桥接：DataQ 48:88xxxx -> 扶摇 88xxxx.TI

const fuyaoBaseURL = "https://fuyao.aicubes.cn"

// fuyaoMembers 查同花顺行业指数的当前成分股，返回 6 位股票代码列表。
// thscode 形如 "881101.TI"。
func fuyaoMembers(thscode string) ([]string, error) {
	key := os.Getenv("FUYAO_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("FUYAO_API_KEY not set")
	}
	fullURL := fuyaoBaseURL + "/api/a-share-index/constituents/ths-stock-list?thscode=" + url.QueryEscape(thscode)
	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-api-key", key)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var d struct {
		Code int `json:"code"`
		Data struct {
			Item []struct {
				Ticker string `json:"ticker"`
			} `json:"item"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	if d.Code != 0 {
		return nil, fmt.Errorf("fuyao code=%d", d.Code)
	}
	var codes []string
	for _, it := range d.Data.Item {
		if it.Ticker != "" {
			codes = append(codes, it.Ticker)
		}
	}
	return codes, nil
}

// dataqToThscode 桥接：DataQ "48:881164" -> 扶摇 "881164.TI"
func dataqToThscode(dataqCode string) string {
	c := dataqCode
	if i := strings.Index(c, ":"); i >= 0 {
		c = c[i+1:]
	}
	return c + ".TI"
}
