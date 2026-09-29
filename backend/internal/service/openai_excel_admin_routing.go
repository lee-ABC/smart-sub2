package service

import (
	"context"
	"fmt"
	"strings"
)

// Admin tests use current account membership, never a forged user API key.
type excelAccountTestRoute struct {
	Transport      string `json:"transport"`
	GroupID        int64  `json:"group_id"`
	AccountID      int64  `json:"account_id"`
	requestedGroup int64
	requestedModel string
}
type excelAccountTestRouteKey struct{}

func prepareExcelAccountTestContext(ctx context.Context, account *Account, requestedGroup int64, model string) (context.Context, *excelAccountTestRoute, error) {
	if account == nil {
		return ctx, nil, fmt.Errorf("account unavailable")
	}
	if requestedGroup < 0 {
		return ctx, nil, fmt.Errorf("test group ID must be positive")
	}
	if previous, ok := ctx.Value(excelAccountTestRouteKey{}).(*excelAccountTestRoute); ok && previous.AccountID == account.ID && previous.requestedGroup == requestedGroup && previous.requestedModel == model {
		return ctx, previous, nil
	}
	info := &excelAccountTestRoute{Transport: "native", AccountID: account.ID, requestedGroup: requestedGroup, requestedModel: model}
	if !account.IsOpenAI() {
		return ctx, info, nil
	}
	groups := make(map[int64]bool)
	for _, id := range account.GroupIDs {
		if id > 0 {
			groups[id] = true
		}
	}
	for _, group := range account.AccountGroups {
		if group.GroupID > 0 {
			groups[group.GroupID] = true
		}
	}
	for _, group := range account.Groups {
		if group != nil && group.ID > 0 {
			groups[group.ID] = true
		}
	}
	if requestedGroup > 0 && !groups[requestedGroup] {
		return ctx, nil, fmt.Errorf("该账号不属于测试分组 #%d，请刷新后重新选择", requestedGroup)
	}
	ctx = context.WithValue(ctx, excelRequestRouteKey{}, (*excelRequestRoute)(nil))
	eligible := account.IsOpenAIOAuthLike() && (strings.TrimSpace(model) == "" || IsExcelSupportedModel(excelRoutingModel(account, model, "")))
	if !eligible {
		info.GroupID = requestedGroup
		if requestedGroup == 0 && len(groups) == 1 {
			for id := range groups {
				info.GroupID = id
			}
		}
		if account.Type == AccountTypeAPIKey {
			info.Transport = "account_upstream"
		}
		return context.WithValue(ctx, excelAccountTestRouteKey{}, info), info, nil
	}
	cfg, err := ReadExcelRoutingConfig()
	if err != nil {
		return ctx, nil, err
	}
	info.GroupID = requestedGroup
	if requestedGroup == 0 {
		for id := range groups {
			if len(groups) == 1 {
				info.GroupID = id
			} else if cfg.Mode(id) == "excel" {
				return ctx, nil, fmt.Errorf("该账号属于多个分组且包含 Excel 通道，请先选择测试分组")
			}
		}
	}
	routed, err := prepareExcelGroupContext(ctx, cfg, account, info.GroupID, 0, "account-test")
	if err != nil {
		return ctx, nil, err
	}
	if isExcelRequest(routed) {
		info.Transport = "excel"
	} else if account.Type == AccountTypeAPIKey {
		info.Transport = "account_upstream"
	}
	return context.WithValue(routed, excelAccountTestRouteKey{}, info), info, nil
}

func excelAccountTestModel(ctx context.Context, model string) string {
	if isExcelRequest(ctx) && strings.TrimSpace(model) == "" {
		return "gpt-6-sol"
	}
	return model
}

// ExcelAccountTestModels performs the same membership and readiness checks as
// execution; discovery never silently falls back to a different transport.
func ExcelAccountTestModels(ctx context.Context, account *Account, groupID int64) ([]string, bool, error) {
	_, route, err := prepareExcelAccountTestContext(ctx, account, groupID, "")
	if err != nil {
		return nil, false, err
	}
	if route.Transport != "excel" {
		return nil, false, nil
	}
	return ExcelModelIDs(), true, nil
}
