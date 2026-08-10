# Stock Filter V9 (Go)

实时优质股筛选系统 V9 的 Go 语言实现。原项目为 Python/Flask（`app_v9.py` / `app_v9_multi.py`），本仓库用 Go 1.22 + 纯标准库完整复刻，包含两个服务：

| 服务 | 端口 | 复刻自 | 功能 |
|---|---|---|---|
| `stock-scanner` | 5800 | `app_v9.py` (5000) | 实时优质股筛选：板块→成分股→财务/预告→垃圾过滤→HMM位置 |
| `stock-portfolio` | 18088 | `app_v9_multi.py` (5001) | 多条件筛选：全市场沪深主板扫描→HMM动态刻度筛选，30分钟自动刷新 |

## 目录结构

```
stock-filter-go/
├── stocklib/          # 共享库：东财/腾讯/新浪数据源、HMM 计算、缓存
├── stock-scanner/     # 5800 端口服务（实时优质股筛选 V9）
└── stock-portfolio/   # 18088 端口服务（多条件筛选 V9）
```

## 构建与运行

```bash
# 构建两个服务
cd stock-scanner && go build -o stock-scanner .
cd ../stock-portfolio && go build -o stock-portfolio .

# 分别启动（各自目录下运行，页面/缓存为相对路径）
cd stock-scanner && ./stock-scanner        # 监听 5800
cd ../stock-portfolio && ./stock-portfolio # 监听 18088
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

## 数据源说明

- 东财 K 线域名（`push2his.eastmoney.com`）在某些网络不可达，故 K 线统一走**腾讯** `web.ifzq.gtimg.cn`（qfq 前复权）
- 东财列表/行情 `push2delay.eastmoney.com` 与财务 `datacenter-web.eastmoney.com` 正常
- 静态资源 ECharts 随仓库分发（`stock-scanner/static/` 与 `stock-portfolio/static/`），离线可用

## 协议

本项目代码 MIT License；ECharts 为 Apache-2.0 许可（见 `static/echarts.min.js` 自带声明）。

## 致谢与边界

- 纯 Go 标准库实现，无第三方 Go 依赖
- 与原 Python 版接口字段、页面、HMM 算法逐项对齐
- 数据源为免费公开接口，可能存在限流/变动，请合理控制请求频率（扫描节流参数已内置）