package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// A new temporary failure arriving during recovery must survive cache cleanup.
func (c *tempUnschedCache) DeleteTempUnschedIfMatches(ctx context.Context, id int64, expected *service.TempUnschedState) error {
	if expected == nil || !service.IsGeminiWebCookieRejection(expected.ErrorMessage) {
		return nil
	}
	payload, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	return c.rdb.Eval(ctx, `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`, []string{fmt.Sprintf("%s%d", tempUnschedPrefix, id)}, string(payload)).Err()
}
