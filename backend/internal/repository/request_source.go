package repository

import "github.com/Wei-Shaw/sub2api/internal/service"

func normalizeRequestSource(source string) string {
	if source == service.RequestSourceProbe {
		return source
	}
	return service.RequestSourceBusiness
}
