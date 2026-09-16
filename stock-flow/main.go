package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============ 数据结构 ============

type SectorData struct {
	Code       string  `json:"code"`
	Name       string  `json:"name"`
	NetInflow  float64 `json:"net_inflow"`
	Inflow     float64 `json:"inflow"`
	Outflow    float64 `json:"outflow"`
	ChangePct  float64 `json:"change_pct"`
	BigOrder   float64 `json:"big_order"`
	Turnover   float64 `json:"turnover"`
	Change5d   float64 `json:"change_5d"`
	Change10d  float64 `json:"change_10d"`
	Efficiency  float64 `json:"efficiency"`
	Composite   float64 `json:"composite"`
	Score       float64 `json:"score"`
	TotalShare  float64 `json:"total_share"`
}

type FlowEdge struct {
	From     string  `json:"from"`
	FromCode string  `json:"from_code"`
	To       string  `json:"to"`
	ToCode   string  `json:"to_code"`
	Flow     float64 `json:"flow"`
	FlowPct  float64 `json:"flow_pct"`
}

type FlowNode struct {
	Name      string  `json:"name"`
	Code      string  `json:"code"`
	NetInflow float64 `json:"net_inflow"`
	Inflow    float64 `json:"inflow"`
	Outflow   float64 `json:"outflow"`
	ChangePct float64 `json:"change_pct"`
	Change5d  float64 `json:"change_5d"`
	Change10d float64 `json:"change_10d"`
	Turnover  float64 `json:"turnover"`
	IsSource  bool    `json:"is_source"`
	IsSink    bool    `json:"is_sink"`
}

type FlowData struct {
	GeneratedAt  string        `json:"generated_at"`
	Sectors      []SectorData  `json:"sectors"`
	OutflowTop   []FlowNode    `json:"outflow_top"`
	InflowTop    []FlowNode    `json:"inflow_top"`
	Edges        []FlowEdge    `json:"edges"`
	TotalInflow  float64       `json:"total_inflow"`
	TotalOutflow float64       `json:"total_outflow"`
	TotalNet     float64       `json:"total_net"`
}

// ============ 全局 ============

var (
	cookie     string
	cacheMutex sync.RWMutex
	lastCache  *FlowData
	cacheTime  time.Time
	cacheTTL   = 30 * time.Second
)

func loadCookie() string {
	data, err := os.ReadFile("/opt/stock-filter-go/stock-flow/cookie.txt")
	if err != nil {
		log.Fatal("cannot read cookie file: ", err)
	}
	return strings.TrimSpace(string(data))
}

// ============ API 调用 ============

func fetchSectors() ([]SectorData, error) {
	payload := `{
		"code_selectors": {
			"intersection": [
				{"type": "tag", "values": ["cn_concept", "industry_l1"]}
			]
		},
		"indexes": [
			{"index_id": "security_name"},
			{"index_id": "inr-main_capital_net_inflow-sum", "time_type": "DAY_1", "timestamp": "0", "attribute": {"win_size": 1}},
			{"index_id": "price_change_ratio_pct"},
			{"index_id": "big_volume_net_ratio", "time_type": "DAY_1", "timestamp": "0"},
			{"index_id": "inr-main_capital_inflow-sum", "time_type": "DAY_1", "timestamp": "0", "attribute": {"win_size": 1}},
			{"index_id": "inr-main_capital_outflow-sum", "time_type": "DAY_1", "timestamp": "0", "attribute": {"win_size": 1}},
			{"index_id": "inr-price_change_ratio_pct-sum", "attribute": {"win_size": 5}, "time_type": "DAY_1", "timestamp": "0"},
			{"index_id": "inr-price_change_ratio_pct-sum", "attribute": {"win_size": 10}, "time_type": "DAY_1", "timestamp": "0"},
			{"index_id": "inr-total_share_cnt-sum", "time_type": "DAY_1", "timestamp": "0", "attribute": {"win_size": 1}},
			{"index_id": "turnover"}
		],
		"page_info": {"page_begin": 0, "page_size": 500, "code_begin": 0},
		"sort": [{"idx": 1, "type": "DESC"}]
	}`

	req, err := http.NewRequest("POST",
		"https://dataq.10jqka.com.cn/fetch-data-server/fetch/v1/specific_data",
		strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Platform", "mobileweb")
	req.Header.Set("Source-id", "mainForce")
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", "dataq.10jqka.com.cn")
	req.Header.Set("User-Agent", "okhttp/4.9.0")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("Accept-Encoding", "identity")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		Data struct {
			Indexes []struct {
				IndexID   string `json:"index_id"`
				Attribute struct {
					WinSize int `json:"win_size"`
				} `json:"attribute"`
			} `json:"indexes"`
			Data []struct {
				Code   string          `json:"code"`
				Values []struct {
					Idx   int             `json:"idx"`
					Value json.RawMessage `json:"value"`
				} `json:"values"`
			} `json:"data"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	// idx → index_id 映射
	idxToID := make(map[int]string)
	for i, idx := range result.Data.Indexes {
		idxToID[i] = idx.IndexID
	}

	// idx → win_size 映射（按数组下标，区分重复 index_id）
	idxToWinSize := make(map[int]int)
	for i, idx := range result.Data.Indexes {
		idxToWinSize[i] = idx.Attribute.WinSize
	}

	var sectors []SectorData
	for _, item := range result.Data.Data {
		s := SectorData{Code: item.Code}
		for _, v := range item.Values {
			id := idxToID[v.Idx]
			ws := idxToWinSize[v.Idx]

			switch id {
			case "security_name":
				var name string
				json.Unmarshal(v.Value, &name)
				s.Name = name
			case "inr-main_capital_net_inflow-sum":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.NetInflow = val
			case "price_change_ratio_pct":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.ChangePct = val
			case "big_volume_net_ratio":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.BigOrder = val
			case "inr-main_capital_inflow-sum":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.Inflow = val
			case "inr-main_capital_outflow-sum":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.Outflow = val
			case "inr-price_change_ratio_pct-sum":
				var val float64
				json.Unmarshal(v.Value, &val)
				if ws == 5 {
					s.Change5d = val
				} else if ws == 10 {
					s.Change10d = val
				}
			case "inr-total_share_cnt-sum":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.TotalShare = val
			case "turnover":
				var val float64
				json.Unmarshal(v.Value, &val)
				s.Turnover = val
			}
		}
		if s.Name != "" {
			if s.BigOrder != 0 {
				s.Efficiency = s.ChangePct / s.BigOrder
			}
			s.Composite = (s.ChangePct + s.Change5d + s.Change10d) / 3
			s.Score = (s.Efficiency + s.Composite) / 2
			sectors = append(sectors, s)
		}
	}
	return sectors, nil
}

// ============ 流向算法 ============

func computeFlow(sectors []SectorData) *FlowData {
	var sources, sinks []SectorData
	for _, s := range sectors {
		if s.NetInflow < 0 && s.Outflow > 0 {
			sources = append(sources, s)
		}
		if s.NetInflow > 0 && s.Inflow > 0 {
			sinks = append(sinks, s)
		}
	}

	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Outflow > sources[j].Outflow
	})
	sort.Slice(sinks, func(i, j int) bool {
		return sinks[i].Inflow > sinks[j].Inflow
	})

	topN := 30
	if len(sources) > topN {
		sources = sources[:topN]
	}
	if len(sinks) > topN {
		sinks = sinks[:topN]
	}

	var edges []FlowEdge
	for _, src := range sources {
		remaining := src.Outflow
		sortedSinks := make([]SectorData, len(sinks))
		copy(sortedSinks, sinks)
		sort.Slice(sortedSinks, func(i, j int) bool {
			return sortedSinks[i].Inflow*(1+sortedSinks[i].ChangePct/100) >
				sortedSinks[j].Inflow*(1+sortedSinks[j].ChangePct/100)
		})

		for _, sink := range sortedSinks {
			if remaining <= 0 {
				break
			}
			flow := remaining
			if flow > sink.Inflow {
				flow = sink.Inflow
			}
			if flow < 10000 {
				continue
			}
			flowPct := flow / src.Outflow * 100
			edges = append(edges, FlowEdge{
				From:     src.Name,
				FromCode: src.Code,
				To:       sink.Name,
				ToCode:   sink.Code,
				Flow:     flow,
				FlowPct:  flowPct,
			})
			remaining -= flow
		}
	}

	outflowTop := make([]FlowNode, len(sources))
	for i, s := range sources {
		outflowTop[i] = FlowNode{
			Name: s.Name, Code: s.Code, NetInflow: s.NetInflow,
			Inflow: s.Inflow, Outflow: s.Outflow, ChangePct: s.ChangePct,
			Change5d: s.Change5d, Change10d: s.Change10d, Turnover: s.Turnover,
			IsSource: true,
		}
	}

	inflowTop := make([]FlowNode, len(sinks))
	for i, s := range sinks {
		inflowTop[i] = FlowNode{
			Name: s.Name, Code: s.Code, NetInflow: s.NetInflow,
			Inflow: s.Inflow, Outflow: s.Outflow, ChangePct: s.ChangePct,
			Change5d: s.Change5d, Change10d: s.Change10d, Turnover: s.Turnover,
			IsSink: true,
		}
	}

	var totalIn, totalOut, totalNet float64
	for _, s := range sectors {
		totalIn += s.Inflow
		totalOut += s.Outflow
		totalNet += s.NetInflow
	}

	return &FlowData{
		GeneratedAt:  time.Now().Format("2006-01-02 15:04:05"),
		Sectors:      sectors,
		OutflowTop:   outflowTop,
		InflowTop:    inflowTop,
		Edges:        edges,
		TotalInflow:  totalIn,
		TotalOutflow: totalOut,
		TotalNet:     totalNet,
	}
}

// ============ HTTP ============

func flowHandler(w http.ResponseWriter, r *http.Request) {
	cacheMutex.RLock()
	if lastCache != nil && time.Since(cacheTime) < cacheTTL {
		cacheMutex.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lastCache)
		return
	}
	cacheMutex.RUnlock()

	sectors, err := fetchSectors()
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusBadGateway)
		return
	}

	flow := computeFlow(sectors)

	cacheMutex.Lock()
	lastCache = flow
	cacheTime = time.Now()
	cacheMutex.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(flow)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "/opt/stock-filter-go/stock-flow/templates/index.html")
}

func main() {
	cookie = loadCookie()
	log.Printf("Cookie loaded, length: %d", len(cookie))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/flow", flowHandler)
	mux.HandleFunc("/", indexHandler)

	fs := http.FileServer(http.Dir("/opt/stock-filter-go/stock-flow/static"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))

	addr := ":18091"
	log.Printf("Stock Flow server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}