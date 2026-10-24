package service

// resolveBillableImageCount 决定一次图片请求按几张计费。
//
// 优先使用上游实际返回的图片张数；流式解析、兼容上游未返回 data 等情况下
// 拿不到实际张数时，回退到客户端请求的 n，避免 n>1 的请求被按 0 张或 1 张漏计。
func resolveBillableImageCount(reported, requested int) int {
	if reported > 0 {
		return reported
	}
	if requested > 0 {
		return requested
	}
	return 1
}
