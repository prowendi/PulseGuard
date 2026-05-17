package web

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendi/pulseguard/internal/condeval"
	"github.com/wendi/pulseguard/internal/domain"
)

// buildUIBindings is the non-HTTP twin of buildBindings: it validates a
// list of template IDs against the tenant and assembles a ChannelTemplate
// slice. conditions, when supplied, applies positionally — conditions[i]
// becomes binding[i].Condition. Errors are returned with friendly Chinese
// messages so the UI flash can surface them verbatim.
func buildUIBindings(ctx context.Context, deps Deps, tenantID int64, templateIDs []int64, defaultID int64, conditions []string) ([]*domain.ChannelTemplate, error) {
	if len(templateIDs) == 0 {
		return nil, fmt.Errorf("请至少选择一个模板")
	}
	seen := map[int64]bool{}
	firstIdx := map[int64]int{}
	clean := make([]int64, 0, len(templateIDs))
	for i, id := range templateIDs {
		if id == 0 {
			return nil, fmt.Errorf("template_id 不能为 0")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		firstIdx[id] = i
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
	// Pre-validate every non-empty condition string so the operator
	// catches typos at submit time instead of via silent worker drift.
	for i, raw := range conditions {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if _, err := condeval.Parse(s); err != nil {
			return nil, fmt.Errorf("条件 #%d 解析失败: %s", i+1, err.Error())
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
		cond := ""
		if idx := firstIdx[id]; idx < len(conditions) {
			cond = strings.TrimSpace(conditions[idx])
		}
		out = append(out, &domain.ChannelTemplate{
			TemplateID: id,
			IsDefault:  isDefault,
			SortOrder:  i,
			Condition:  cond,
		})
	}
	return out, nil
}
