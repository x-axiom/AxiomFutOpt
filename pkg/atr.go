package pkg

// 计算某数据的最近n天的ATR%
//
// path为数据的csv文件目录, csv文件格式符合:
//
// date,open,high,low,close
// 2005-01-04,996.28,996.28,982.99,989.98
// 2005-01-05,989.87,1018.26,988.57,1013.58
// 2005-01-06,1014.97,1014.97,1001.21,1005.47
func AtrOut(n int, path string) {

}

// $$TR_t = \max(H_t - L_t, \ |H_t - C_{t-1}|, \ |L_t - C_{t-1}|)$$
func atr(high, low, closePrevious float64) float64 {
	return 0
}
