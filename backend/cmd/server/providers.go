package main

import (
	"github.com/google/wire"
	"sub2api-enhance/internal/config"
	"sub2api-enhance/internal/notify"
	audit "sub2api-enhance/internal/thirdpartypromptaudit"
)

type Core struct {
	Service *audit.Service
	Handler *audit.AdminHandler
}

// coreProviders 连接配置加密、通知与审核模块的实际独立依赖。
var coreProviders = wire.NewSet(config.NewEncryptor, wire.Bind(new(config.SecretEncryptor), new(*config.Encryptor)), notify.NewSMTP, wire.Bind(new(notify.Sender), new(*notify.SMTP)), audit.ProviderSet, wire.Struct(new(Core), "*"))
