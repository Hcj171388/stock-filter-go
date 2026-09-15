package main

import (
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"stocklib"
)

// 产业链字典：8 条核心链，核心词→{上游/中游/下游: [板块名关键词]}
// 关键词对东财板块名做 strings.Contains 匹配，命中即归入对应角色

type ChainRole struct {
	Role  string   `json:"role"`
	Names []string `json:"names"`
}

type ChainLink struct {
	Core    string      `json:"core"`
	RoleMap []ChainRole `json:"role_map"`
}

var chainDict = []ChainLink{
	{Core: "半导体", RoleMap: []ChainRole{
		{"上游", []string{"半导体设备", "光刻胶", "半导体材料", "电子化学品", "封装测试", "封测", "靶材", "硅片", "硅料"}},
		{"中游", []string{"芯片", "半导体", "集成电路", "模拟", "数字芯片", "存储", "国产"}},
		{"下游", []string{"消费电子", "汽车电子", "服务器", "算力", "品牌消费电子"}},
	}},
	{Core: "AI算力", RoleMap: []ChainRole{
		{"上游", []string{"光模块", "光通信", "PCB", "印制电路板", "GPU", "光芯片", "半导体设备"}},
		{"中游", []string{"算力", "人工智能", "AI", "大模型", "数据中心", "液冷", "服务器", "通信设备", "通信"}},
		{"下游", []string{"应用", "传媒", "游戏", "机器人", "智能驾驶", "自动驾驶", "垂直应用"}},
	}},
	{Core: "新能源车", RoleMap: []ChainRole{
		{"上游", []string{"锂电", "电池化学品", "锂", "镍", "钴", "稀土", "正极", "负极", "电解液", "隔膜"}},
		{"中游", []string{"电池", "动力电池", "整车", "新能源汽车", "电动乘用车", "电机", "电控", "燃料电池"}},
		{"下游", []string{"充电桩", "换电", "汽车", "汽车零部件", "汽车电子", "智能驾驶", "智能网联"}},
	}},
	{Core: "光伏", RoleMap: []ChainRole{
		{"上游", []string{"硅料", "硅片", "光伏主材", "石英", "多晶硅", "靶材", "光伏辅材"}},
		{"中游", []string{"光伏", "电池组件", "逆变器", "光伏发电"}},
		{"下游", []string{"储能", "电力", "电网", "风电", "绿电", "发电"}},
	}},
	{Core: "军工", RoleMap: []ChainRole{
		{"上游", []string{"特种材料", "碳纤维", "钛合金", "稀土", "磁性材料"}},
		{"中游", []string{"军工", "航天", "航空", "船舶", "兵器", "国防", "兵装"}},
		{"下游", []string{"卫星", "北斗", "低空", "无人机", "导弹", "雷达", "航空装备"}},
	}},
	{Core: "机器人", RoleMap: []ChainRole{
		{"上游", []string{"减速器", "伺服", "传感器", "控制器", "电机", "工业"}},
		{"中游", []string{"机器人", "工业自动化", "机械", "协作"}},
		{"下游", []string{"汽车", "消费电子", "服务机器人"}},
	}},
	{Core: "创新药", RoleMap: []ChainRole{
		{"上游", []string{"原料药", "药用辅料", "生命科学", "CRO", "CDMO", "CXO"}},
		{"中游", []string{"创新药", "生物医药", "疫苗", "基因", "细胞", "生物", "中药"}},
		{"下游", []string{"医疗服务", "医疗器械", "诊断", "体外诊断", "康复"}},
	}},
	{Core: "消费电子", RoleMap: []ChainRole{
		{"上游", []string{"面板", "光学", "摄像头", "连接器", "触控", "光学光电子", "光学元件"}},
		{"中游", []string{"消费电子", "品牌消费电子", "手机", "智能硬件"}},
		{"下游", []string{"可穿戴", "VR", "AR", "XR", "眼镜"}},
	}},
}

func keywordFuzzyChain(core string, inds []IndustryInfo) []string {
	out := []string{}
	for _, in := range inds {
		if strings.Contains(in.Name, core) && in.Name != core {
			out = append(out, in.Name)
		}
	}
	return out
}

// findChainCore 判断板块名属于哪条核心产业链（板块名包含核心词，或核心词包含于板块名）
func findChainCore(name string) string {
	for _, c := range chainDict {
		if strings.Contains(name, c.Core) || strings.Contains(c.Core, name) {
			return c.Core
		}
	}
	return ""
}

// ============ 双榜 + 板块成分股 ============
// 涨幅榜/净流入榜各取前N，去重合并；5分钟缓存
// 板块成分股按需懒加载（b:BK代码），磁盘缓存，点击时查

type SectorBoard struct {
	Code   string   `json:"code"`
	Name   string   `json:"name"`
	Change *float64 `json:"change"`
	NetIn  *float64 `json:"netin"`
	Ratio  *float64 `json:"ratio"`
}

type ChainPanel struct {
	ByGain    []SectorBoard `json:"by_gain"`
	ByNetIn   []SectorBoard `json:"by_netin"`
	TTL       int           `json:"ttl"`
	Generated int64         `json:"generated"`
}

var sectorsCacheFile = filepath.Join(CACHE_DIR, "top_sectors_x.json")

func sectorPanelCache() (*ChainPanel, bool) {
	var p ChainPanel
	if ok, _ := stocklib.CacheCheckAge(sectorsCacheFile, SECTORS_TTL_SEC); ok {
		if err := stocklib.CacheReadJSON(sectorsCacheFile, &p); err == nil && len(p.ByGain)+len(p.ByNetIn) > 0 {
			return &p, true
		}
	}
	return nil, false
}

func topSectorsDual() (*ChainPanel, error) {
	if p, ok := sectorPanelCache(); ok {
		return p, nil
	}
	byGain := make([]SectorBoard, 0, TOP_PANEL_N)
	byNetIn := make([]SectorBoard, 0, TOP_PANEL_N)
	for _, sortKey := range []string{"f3", "f62"} {
		seen := map[string]bool{}
		for pn := 1; ; pn++ {
			data, err := stocklib.FetchEM(map[string]string{
				"pn": strconv.Itoa(pn), "pz": "100", "fid": sortKey,
				"fs": INDUSTRY_FS, "fields": "f12,f14,f3,f62",
			})
			if err != nil {
				log.Printf("[sector] 板块榜第%d页失败(fid=%s): %v", pn, sortKey, err)
				break
			}
			for _, it := range data.Data.Diff {
				name, _ := it["f14"].(string)
				if name == "" {
					continue
				}
				code, _ := it["f12"].(string)
				if seen[code] {
					continue
				}
				sb := SectorBoard{
					Code: code, Name: name,
					Change: toFloatPtr(it["f3"]),
				}
				if raw := toFloatPtr(it["f62"]); raw != nil {
					yi := round2(*raw / 1e8)
					sb.NetIn = &yi
				}
				seen[code] = true
				if sortKey == "f3" {
					if len(byGain) < TOP_PANEL_N {
						byGain = append(byGain, sb)
					}
				} else {
					// 净流入榜：只保留净额>0
					if sb.NetIn != nil && *sb.NetIn > 0 && len(byNetIn) < TOP_PANEL_N {
						byNetIn = append(byNetIn, sb)
					}
				}
			}
			if len(data.Data.Diff) == 0 || pn*100 >= data.Data.Total {
				break
			}
		}
	}
	// 占比
	for i := range byGain {
		r := sectorRatio(byGain[i])
		byGain[i].Ratio = r
	}
	for i := range byNetIn {
		r := sectorRatio(byNetIn[i])
		byNetIn[i].Ratio = r
	}
	p := &ChainPanel{ByGain: byGain, ByNetIn: byNetIn, TTL: SECTORS_TTL_SEC, Generated: time.Now().Unix()}
	stocklib.CacheWriteJSON(sectorsCacheFile, p)
	return p, nil
}

func sectorRatio(sb SectorBoard) *float64 {
	if sb.Change == nil || sb.NetIn == nil {
		return nil
	}
	denom := *sb.NetIn
	if denom == 0 {
		denom = 1e-10
	}
	r := round2(*sb.Change / denom)
	return &r
}

// 板块成分股：按 b:BK代码 拉（pz=500），磁盘缓存 5 分钟
func sectorMembers(code string) []string {
	if code == "" {
		return nil
	}
	fname := filepath.Join(CACHE_DIR, "members_"+code+".json")
	type mcache struct {
		Codes []string `json:"codes"`
		T     int64    `json:"t"`
	}
	var c mcache
	if ok, _ := stocklib.CacheCheckAge(fname, SECTORS_TTL_SEC); ok {
		if err := stocklib.CacheReadJSON(fname, &c); err == nil && c.Codes != nil {
			return c.Codes
		}
	}
	var codes []string
	for pn := 1; pn <= 5; pn++ {
		data, err := stocklib.FetchEM(map[string]string{
			"pn": strconv.Itoa(pn), "pz": "500", "fid": "f3",
			"fs": "b:" + code, "fields": "f12,f14",
		})
		if err != nil {
			log.Printf("[members] %s 第%d页失败: %v", code, pn, err)
			break
		}
		for _, it := range data.Data.Diff {
			if v, ok2 := it["f12"].(string); ok2 && v != "" {
				codes = append(codes, v)
			}
		}
		if len(data.Data.Diff) == 0 || len(codes) >= data.Data.Total {
			break
		}
	}
	c = mcache{Codes: codes, T: time.Now().Unix()}
	stocklib.CacheWriteJSON(fname, &c)
	return codes
}
