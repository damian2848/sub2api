package service

// resolveBillableImageCount 决定一次图片请求按几张计费。
//
// 官方多图响应没有 n 字段，只以响应体 data/output 中实际返回的图片为准。
// 解析不到张数时按单张兜底，避免已成功出图的请求完全漏计。
func resolveBillableImageCount(reported int) int {
	if reported > 0 {
		return reported
	}
	return 1
}
