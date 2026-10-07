package service

import (
	"context"
)

type AccountOpsNotificationSettings struct {
	Enabled         *bool   `json:"enabled"`
	Recipient       *string `json:"recipient"`
	BalanceLow      *bool   `json:"balance_low"`
	WeeklyQuota     *bool   `json:"weekly_quota"`
	CooldownMinutes *int    `json:"cooldown_minutes"`
}
type AccountOpsRuleUpdate struct {
	Metric           string   `json:"metric"`
	Enabled          bool     `json:"enabled"`
	Threshold        *float64 `json:"threshold"`
	Unit             string   `json:"unit"`
	ThresholdPercent *float64 `json:"threshold_percent"`
	Window           string   `json:"window"`
	NotifyAlert      *bool    `json:"notify_alert"`
	NotifyRecovery   *bool    `json:"notify_recovery"`
}
type opsConfigResultKey struct{}
type opsConfigResult struct {
	cfg AccountOpsConfig
	set bool
}
type accountOpsConfigLocker interface {
	WithAccountOpsConfigLock(context.Context, func(context.Context) error) error
}

func (s *AccountOpsService) updateConfig(ctx context.Context, mutate func(*AccountOpsConfig) error) (AccountOpsConfig, error) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	var out AccountOpsConfig
	result := &opsConfigResult{}
	ctx = context.WithValue(ctx, opsConfigResultKey{}, result)
	update := func(lockedCtx context.Context) error {
		ctx = lockedCtx
		c, err := s.loadConfig(ctx)
		if err != nil {
			return err
		}
		if err = mutate(&c); err != nil {
			return err
		}
		if err = s.saveConfigWithRuleScope(ctx, c, false); err != nil {
			return err
		}
		return nil
	}
	var err error
	if lock, ok := s.repo.(accountOpsConfigLocker); ok {
		err = lock.WithAccountOpsConfigLock(ctx, update)
	} else {
		err = update(ctx)
	}
	if err == nil && result.set {
		s.config.Store(result.cfg)
		out = result.cfg.public()
	}
	return out, err
}
func (s *AccountOpsService) SaveNotificationSettings(ctx context.Context, v AccountOpsNotificationSettings) (AccountOpsConfig, error) {
	return s.updateConfig(ctx, func(c *AccountOpsConfig) error {
		if v.Enabled != nil {
			c.Enabled = *v.Enabled
		}
		if v.Recipient != nil {
			c.Recipient = *v.Recipient
		}
		if v.BalanceLow != nil {
			c.BalanceLow = *v.BalanceLow
		}
		if v.WeeklyQuota != nil {
			c.WeeklyQuota = *v.WeeklyQuota
		}
		if v.CooldownMinutes != nil {
			c.CooldownMinutes = *v.CooldownMinutes
		}
		return nil
	})
}
func (s *AccountOpsService) SaveWebhook(ctx context.Context, id string, v AccountOpsWebhook) (AccountOpsConfig, error) {
	if !opsID.MatchString(id) {
		return AccountOpsConfig{}, accountOpsConfigValidation("invalid robot ID")
	}
	return s.updateConfig(ctx, func(c *AccountOpsConfig) error {
		v.ID = id
		for i, w := range c.Webhooks {
			if w.ID == id {
				c.Webhooks[i] = v
				return nil
			}
		}
		c.Webhooks = append(c.Webhooks, v)
		return nil
	})
}
func (s *AccountOpsService) DeleteWebhook(ctx context.Context, id string) (AccountOpsConfig, error) {
	if !opsID.MatchString(id) {
		return AccountOpsConfig{}, accountOpsConfigValidation("invalid robot ID")
	}
	return s.updateConfig(ctx, func(c *AccountOpsConfig) error {
		for i, w := range c.Webhooks {
			if w.ID == id {
				c.Webhooks = append(c.Webhooks[:i], c.Webhooks[i+1:]...)
				break
			}
		}
		return nil
	})
}
func (s *AccountOpsService) SaveRule(ctx context.Context, id int64, v AccountOpsRuleUpdate) (AccountOpsConfig, error) {
	if id <= 0 || (v.Metric != "balance" && v.Metric != "quota") {
		return AccountOpsConfig{}, accountOpsConfigValidation("invalid threshold rule")
	}
	return s.updateConfig(ctx, func(c *AccountOpsConfig) error {
		if v.Metric == "balance" {
			r := AccountOpsBalanceRule{AccountID: id, Enabled: v.Enabled, NotifyAlert: v.NotifyAlert, NotifyRecovery: v.NotifyRecovery}
			index := -1
			for i, old := range c.BalanceThresholds {
				if old.AccountID == id {
					r = old
					index = i
					break
				}
			}
			if v.Threshold != nil {
				r.Threshold = *v.Threshold
			} else if index < 0 {
				return accountOpsConfigValidation("threshold is required")
			}
			if v.Unit != "" {
				r.Unit = v.Unit
			} else if index < 0 {
				return accountOpsConfigValidation("unit is required")
			}
			r.Enabled = v.Enabled
			if v.NotifyAlert != nil {
				r.NotifyAlert = v.NotifyAlert
			}
			if v.NotifyRecovery != nil {
				r.NotifyRecovery = v.NotifyRecovery
			}
			if err := s.validateBalanceRules(ctx, []AccountOpsBalanceRule{r}); err != nil {
				return err
			}
			if index < 0 {
				c.BalanceThresholds = append(c.BalanceThresholds, r)
			} else {
				c.BalanceThresholds[index] = r
			}
		} else {
			r := AccountOpsQuotaRule{AccountID: id, Enabled: v.Enabled, NotifyAlert: v.NotifyAlert, NotifyRecovery: v.NotifyRecovery}
			index := -1
			for i, old := range c.QuotaThresholds {
				if old.AccountID == id {
					r = old
					index = i
					break
				}
			}
			if v.ThresholdPercent != nil {
				r.ThresholdPercent = *v.ThresholdPercent
			} else if index < 0 {
				return accountOpsConfigValidation("threshold_percent is required")
			}
			if v.Window != "" {
				r.Window = v.Window
			}
			r.Enabled = v.Enabled
			if v.NotifyAlert != nil {
				r.NotifyAlert = v.NotifyAlert
			}
			if v.NotifyRecovery != nil {
				r.NotifyRecovery = v.NotifyRecovery
			}
			if err := s.validateQuotaRules(ctx, []AccountOpsQuotaRule{r}); err != nil {
				return err
			}
			if index < 0 {
				c.QuotaThresholds = append(c.QuotaThresholds, r)
			} else {
				c.QuotaThresholds[index] = r
			}
		}
		return nil
	})
}
func (s *AccountOpsService) DeleteRule(ctx context.Context, id int64, metric string) (AccountOpsConfig, error) {
	if id <= 0 || (metric != "balance" && metric != "quota") {
		return AccountOpsConfig{}, accountOpsConfigValidation("invalid threshold rule")
	}
	return s.updateConfig(ctx, func(c *AccountOpsConfig) error {
		if metric == "balance" {
			for i, r := range c.BalanceThresholds {
				if r.AccountID == id {
					c.BalanceThresholds = append(c.BalanceThresholds[:i], c.BalanceThresholds[i+1:]...)
					break
				}
			}
		} else {
			for i, r := range c.QuotaThresholds {
				if r.AccountID == id {
					c.QuotaThresholds = append(c.QuotaThresholds[:i], c.QuotaThresholds[i+1:]...)
					break
				}
			}
		}
		return nil
	})
}
