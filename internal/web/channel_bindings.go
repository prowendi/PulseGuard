package web

import (
	"context"
	"errors"
	"fmt"

	"github.com/wendi/pulseguard/internal/domain"
)

// buildUIBindings is the non-HTTP twin of buildBindings: it validates a
// list of template IDs against the tenant and assembles a ChannelTemplate
// slice. Errors are returned with friendly Chinese messages so the UI
// flash can surface them verbatim.
func buildUIBindings(ctx context.Context, deps Deps, tenantID int64, templateIDs []int64, defaultID int64) ([]*domain.ChannelTemplate, error) {
	if len(templateIDs) == 0 {
		return nil, fmt.Errorf("请至少选择一个模板")
	}
	seen := map[int64]bool{}
	clean := make([]int64, 0, len(templateIDs))
	for _, id := range templateIDs {
		if id == 0 {
			return nil, fmt.Errorf("template_id 不能为 0")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if defaultID != 0 && !seen[defaultID] {
		return nil, fmt.Errorf("默认模板必须出现在所选模板中")
	}
	for _, id := range clean {
		if _, err := deps.Templates.GetByID(ctx, tenantID, id); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return nil, fmt.Errorf("模板 #%d 不存在或不属于当前租户", id)
			}
			return nil, fmt.Errorf("查模板出错: %w", err)
		}
	}
	out := make([]*domain.ChannelTemplate, 0, len(clean))
	for i, id := range clean {
		isDefault := false
		if defaultID == 0 && i == 0 {
			isDefault = true
		}
		if defaultID != 0 && id == defaultID {
			isDefault = true
		}
		out = append(out, &domain.ChannelTemplate{
			TemplateID: id,
			IsDefault:  isDefault,
			SortOrder:  i,
		})
	}
	return out, nil
}
