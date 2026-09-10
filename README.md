# Stock Filter V9 (Go)

实时优质股筛选系统 V9 的 Go 语言实现。原项目为 Python/Flask（`app_v9.py` / `app_v9_multi.py`），本仓库用 Go 1.22 + 纯标准库完整复刻，并在此基础上扩展，共包含三个服务：

| 服务 | 端口 | 复刻自 | 功能 |
|---|---|---|---|
| `stock-scanner` | 5800 | `app_v9.py` (5000) | 实时优质股筛选：板块→成分股→财务/预告→垃圾过滤→HMM位置 |
| `stock-portfolio` | 18088 | `app_v9_multi.py` (5001) | 多条件筛选：全市场沪深主板扫描→HMM动态刻度筛选，30分钟自动刷新 |
| `stock-select` | 18090 | 独立扩展 | 沪深主板五维选股：行业净额/涨跌 + K线指标 + 业绩 + 资金多维，后端取数算指标、前端全量过滤，30分钟刷新（Nginx 反代 HTTPS 对外） |

## 目录结构

```
stock-filter-go/
├── stocklib/          # 共享库：东财/腾讯/新浪数据源、HMM 计算、缓存
├── stock-scanner/     # 5800 端口服务（实时优质股筛选 V9）
├── stock-portfolio/   # 18088 端口服务（多条件筛选 V9）
└── stock-select/      # 18090 端口服务（沪深主板五维选股，Nginx 反代 HTTPS）
```

## 构建与运行

```bash
# 构建三个服务
cd stock-scanner && go build -o stock-scanner .
cd ../stock-portfolio && go build -o stock-portfolio .
cd ../stock-select && go build -o stock-select .

# 分别启动（各自目录下运行，页面/缓存为相对路径）
cd stock-scanner && ./stock-scanner        # 监听 5800
cd ../stock-portfolio && ./stock-portfolio # 监听 18088
cd ../stock-select && ./stock-select       # 监听 18090
```

启动参数（端口）在各自 `main.go` 顶部的 `PORT` 常量，如需修改直接改端口后重新编译。

## 功能

### stock-scanner (5800)
- 数据源：东方财富 `push2delay` clist（板块/成分股）+ `datacenter-web`（财务/预告）+ 新浪日K（HMM 位置）
- 页面：板块栏 + 9列表格 + HMM 图表弹窗（ECharts）
- 接口：`/stock/api/stocks`、`/stock/api/chart/<code>`、`/stock/api/refresh-cache`、`/api/scan-ports`

### stock-portfolio (18088)
- 全市场沪深主板扫描（约 3483 只），8 并发 + 200/批节流 + 失败重试
- HMM 动态价格刻度底部筛选（👍/👌），财务/预告批量补充
- 后台 goroutine 每 30 分钟自动全量扫描，非交易日（周末）自动跳过
- 图表接口代理转发到 5800 复用其 HMM 计算
- 接口：`/stock/api/multi`（支持 `?fresh=1` 强制重扫）、`/stock/api/chart/<code>`

### stock-select (18090)
- 沪深主板五维选股：行业净额/涨跌 + K线指标(Ind1/Ind2) + 业绩(净利同比/公告日) + 资金多维
- 后端只取数算指标（零筛选），所有筛选逻辑在前端单文件 `page.html` 完成
- 数据源：东财 `push2delay` clist（行情含个股 `f62` 主力净流入 + 行业 `f62` 主力净流入）+ 腾讯 `proxy.finance.qq.com` 单只K线（磁盘缓存 25min TTL）+ 本地 `data/yjgg.jsonl`（10jqka 业绩缓存）
- 后台每 30 分钟自动扫描 + `POST /api/refresh` 手动触发；接口 `/api/snapshot`、`/api/refresh`、`/api/status`
- 页面：个股 13 列表格，净利同比 / 主力净额 / 行业涨跌 / 行业净额 四列红涨绿跌色标 + 前端全量过滤（筛选含「上涨且主力净额<0」「DELTA上穿0」）
- DELTA 指标：`DELTA=(MA(C,40)-MA(C,20))-(MA(C,20)-MA(C,10))-(MA(C,10)-MA(C,5))-(MA(C,5)-MA(C,3))`，基于前复权收盘价；「上穿0」= TDX CROSS 语义（今日 DELTA>0 且 昨日 DELTA≤0），后端算好 `delta`/`delta_up` 随快照下发，前端勾选过滤，K线不足 41 日的股票无值不过筛
- **命中跟踪**：命中 3 条件（净利同比>0 且 指标1≤-7 且 行业净额>0 且 行业涨跌>0）的股票自动记录基准，跟踪其后 10 个交易日；点股票名弹窗直接显示 5日/10日 累计涨跌幅（后台每轮算好渲染，不临时查）

## 数据源说明

- 东财 K 线域名（`push2his.eastmoney.com`）在某些网络不可达，故 K 线统一走**腾讯** `web.ifzq.gtimg.cn`（qfq 前复权）
- 东财列表/行情 `push2delay.eastmoney.com` 与财务 `datacenter-web.eastmoney.com` 正常
- 静态资源 ECharts 随仓库分发（`stock-scanner/static/` 与 `stock-portfolio/static/`），离线可用
- 行业主力净流入取东财 clist `f62`（元，有正负，/1e8 转亿元）；`f106` 在板块场景无负值、已废弃
- 业绩数据走本地缓存 `data/yjgg.jsonl`（10jqka 业绩数据库，每日增量更新）
- 命中跟踪表 `data/hittrack.json`（所有命中股票共享，按代码为 key；未满 11 天保留首次命中基准不重置，满 11 天可重置；跟踪期内逐日补录收盘至多 11 天）

## 协议

本项目代码 MIT License；ECharts 为 Apache-2.0 许可（见 `static/echarts.min.js` 自带声明）。

## 致谢与边界

- 纯 Go 标准库实现，无第三方 Go 依赖
- 与原 Python 版接口字段、页面、HMM 算法逐项对齐
- 数据源为免费公开接口，可能存在限流/变动，请合理控制请求频率（扫描节流参数已内置）