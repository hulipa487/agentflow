package main

import (
	"fmt"
	"path/filepath"
	"sort"

	"agentflow/internal/config"
	"agentflow/internal/core/ext"
)

func resolveAPIPath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) || baseDir == "" {
		return p
	}
	return filepath.Join(baseDir, p)
}

func registerConfiguredAPIs(reg *ext.Registry, apis map[string]config.APIConfig, baseDir string) error {
	if reg == nil || len(apis) == 0 {
		return nil
	}
	names := make([]string, 0, len(apis))
	for name := range apis {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		a := apis[name]
		if a.Dir != "" {
			dir := resolveAPIPath(baseDir, a.Dir)
			m, err := ext.LoadManifestDir(dir)
			if err != nil {
				return fmt.Errorf("apis.%s: %w", name, err)
			}
			m.Name = name
			if err := reg.Register(m, dir); err != nil {
				return fmt.Errorf("apis.%s: %w", name, err)
			}
			continue
		}
		var httpCap *ext.HTTPCapability
		if a.Capabilities.HTTP != nil {
			httpCap = &ext.HTTPCapability{
				AllowedHosts: a.Capabilities.HTTP.AllowedHosts,
				Credentials:  a.Capabilities.HTTP.Credentials,
			}
		}
		actions := make(map[string]ext.ActionSpec, len(a.Actions))
		for actName, act := range a.Actions {
			actions[actName] = ext.ActionSpec{
				Description:        act.Description,
				ExposeAsTool:       act.ExposeAsTool,
				NeedsConfirm:       act.NeedsConfirm,
				Kind:               act.Kind,
				RequiresCapability: act.RequiresCapability,
				Parameters:         act.Parameters,
			}
		}
		m := ext.Manifest{
			Name:        name,
			Version:     a.Version,
			Description: a.Description,
			Runtime:     ext.RuntimeKind(a.Runtime),
			Entry:       resolveAPIPath(baseDir, a.Entry),
			Timeout:     a.Timeout,
			MemoryMB:    a.MemoryMB,
			Capabilities: ext.Capabilities{
				HTTP: httpCap,
			},
			Actions: actions,
		}
		if err := reg.Register(m, ""); err != nil {
			return fmt.Errorf("apis.%s: %w", name, err)
		}
	}
	return nil
}
