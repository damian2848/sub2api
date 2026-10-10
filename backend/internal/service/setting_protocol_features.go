package service

import "context"

// GetProtocolFeatureEnabled reads a database-backed protocol switch on every
// request so administrator changes apply without restarting gateway workers.
func (s *SettingService) GetProtocolFeatureEnabled(ctx context.Context, key string) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, nil
	}
	values, err := s.settingRepo.GetMultiple(ctx, []string{key})
	if err != nil {
		return false, err
	}
	value, exists := values[key]
	if key == SettingKeyExcelBPSEnabled && (!exists || value == "") {
		return true, nil
	}
	return value == "true", nil
}
