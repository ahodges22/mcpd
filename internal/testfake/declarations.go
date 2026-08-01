package testfake

import "github.com/ahodges/mcpd/internal/config"

type PermissiveDeclarations struct{}

func (PermissiveDeclarations) Identity(string) (config.Identity, bool) {
	return config.Identity{}, true
}

func (PermissiveDeclarations) HoldDeclared(_ string, _ *config.Identity, fn func()) bool {
	fn()
	return true
}
