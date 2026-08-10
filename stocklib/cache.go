package stocklib

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"time"
)

// ============ 缓存读写工具 ============

// CacheReadJSON 读取缓存 JSON 到 out（不要求存在）
func CacheReadJSON(file string, out interface{}) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// CacheWriteJSON 原子写 JSON（临时文件 + rename），与原版 _write_main_cache 一致
func CacheWriteJSON(file string, v interface{}) error {
	tmp := file + ".tmp"
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// CacheFileName 复刻原版 hash(cache_key) % 100000 的文件命名。
// 原版用 Python hash()（每次进程随机盐），此处用确定性 FNV，仅用于区分不同代码组合，格式 fin_12345.json
func CacheFileName(prefix, joinedKey string) string {
	h := fnv.New32a()
	h.Write([]byte(joinedKey))
	return filepath.Join("cache", prefix+fmt.Sprint(h.Sum32()%100000)+".json")
}

// CacheCheckAge 判断缓存文件是否在 ttl 秒内
func CacheCheckAge(file string, ttl int64) (bool, time.Time) {
	info, err := os.Stat(file)
	if err != nil {
		return false, time.Time{}
	}
	age := time.Since(info.ModTime()).Seconds()
	if age < float64(ttl) {
		return true, info.ModTime()
	}
	return false, time.Time{}
}