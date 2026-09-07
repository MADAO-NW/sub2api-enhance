package logger

import "log"

// LegacyPrintf 保留迁移模块的日志调用约定，输出中文业务节点及稳定检索标识。
func LegacyPrintf(scope, format string, args ...any) { log.Printf("["+scope+"] "+format, args...) }
