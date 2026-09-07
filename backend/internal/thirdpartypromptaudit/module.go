package thirdpartypromptaudit

import "github.com/google/wire"

// ProviderSet 仅装配独立审核模块，不注册原项目网关或用户事务钩子。
var ProviderSet = wire.NewSet(NewRepository, NewConfigManager, NewModelClient, NewEvaluator, NewService, NewAdminHandler)
