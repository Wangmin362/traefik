package plugins

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/traefik/traefik/v2/pkg/log"
)

const localGoPath = "./plugins-local/"

// SetupRemotePlugins setup remote plugins environment.
// 下载用户配置的所有插件，然后解压缩到指定的目录
func SetupRemotePlugins(client *Client, plugins map[string]Descriptor) error {
	// 检查远程插件的配置
	err := checkRemotePluginsConfiguration(plugins)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// 清理当前即将要下载插件目录
	err = client.CleanArchives(plugins)
	if err != nil {
		return fmt.Errorf("unable to clean archives: %w", err)
	}

	ctx := context.Background()

	for pAlias, desc := range plugins {
		log.FromContext(ctx).Debugf("loading of plugin: %s: %s@%s", pAlias, desc.ModuleName, desc.Version)

		// 下载插件
		hash, err := client.Download(ctx, desc.ModuleName, desc.Version)
		if err != nil {
			// 如果插件下载有问题，就直接清理所有的插件
			_ = client.ResetAll()
			return fmt.Errorf("unable to download plugin %s: %w", desc.ModuleName, err)
		}

		// 校验插件是否下载完成
		err = client.Check(ctx, desc.ModuleName, desc.Version, hash)
		if err != nil {
			// 校验不通过，清理所有的插件
			_ = client.ResetAll()
			return fmt.Errorf("unable to check archive integrity of the plugin %s: %w", desc.ModuleName, err)
		}
	}

	// 把插件配置保存起来
	err = client.WriteState(plugins)
	if err != nil {
		// 保存有问题的话，清理所有的插件
		_ = client.ResetAll()
		return fmt.Errorf("unable to write plugins state: %w", err)
	}

	// 解压缩所有的插件
	for _, desc := range plugins {
		err = client.Unzip(desc.ModuleName, desc.Version)
		if err != nil {
			_ = client.ResetAll()
			return fmt.Errorf("unable to unzip archive: %w", err)
		}
	}

	return nil
}

func checkRemotePluginsConfiguration(plugins map[string]Descriptor) error {
	if plugins == nil {
		return nil
	}

	uniq := make(map[string]struct{})

	var errs []string
	for pAlias, descriptor := range plugins {
		if descriptor.ModuleName == "" {
			errs = append(errs, fmt.Sprintf("%s: plugin name is missing", pAlias))
		}

		if descriptor.Version == "" {
			errs = append(errs, fmt.Sprintf("%s: plugin version is missing", pAlias))
		}

		if strings.HasPrefix(descriptor.ModuleName, "/") || strings.HasSuffix(descriptor.ModuleName, "/") {
			errs = append(errs, fmt.Sprintf("%s: plugin name should not start or end with a /", pAlias))
			continue
		}

		if _, ok := uniq[descriptor.ModuleName]; ok {
			errs = append(errs, fmt.Sprintf("only one version of a plugin is allowed, there is a duplicate of %s", descriptor.ModuleName))
			continue
		}

		uniq[descriptor.ModuleName] = struct{}{}
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ": "))
	}

	return nil
}

// SetupLocalPlugins setup local plugins environment.
func SetupLocalPlugins(plugins map[string]LocalDescriptor) error {
	if plugins == nil {
		return nil
	}

	uniq := make(map[string]struct{})

	var errs *multierror.Error
	for pAlias, descriptor := range plugins {
		if descriptor.ModuleName == "" {
			errs = multierror.Append(errs, fmt.Errorf("%s: plugin name is missing", pAlias))
		}

		// 模块名不能以 / 开头或结尾
		if strings.HasPrefix(descriptor.ModuleName, "/") || strings.HasSuffix(descriptor.ModuleName, "/") {
			errs = multierror.Append(errs, fmt.Errorf("%s: plugin name should not start or end with a /", pAlias))
			continue
		}

		// 插件名不能冲突
		if _, ok := uniq[descriptor.ModuleName]; ok {
			errs = multierror.Append(errs, fmt.Errorf("only one version of a plugin is allowed, there is a duplicate of %s", descriptor.ModuleName))
			continue
		}

		uniq[descriptor.ModuleName] = struct{}{}

		// 加载本地插件，检查该插件的配置
		err := checkLocalPluginManifest(descriptor)
		errs = multierror.Append(errs, err)
	}

	return errs.ErrorOrNil()
}

// 检查插件的配置
func checkLocalPluginManifest(descriptor LocalDescriptor) error {
	// 从./plugins-local/<moduleName>位置中读取插件的配置
	m, err := ReadManifest(localGoPath, descriptor.ModuleName)
	if err != nil {
		return err
	}

	var errs *multierror.Error

	switch m.Type {
	// 从这里可以看出，插件的类型只能是 middleware 或者 provider
	case "middleware", "provider":
		// noop
	default:
		errs = multierror.Append(errs, fmt.Errorf("%s: unsupported type %q", descriptor.ModuleName, m.Type))
	}

	if m.Import == "" {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing import", descriptor.ModuleName))
	}

	if !strings.HasPrefix(m.Import, descriptor.ModuleName) {
		errs = multierror.Append(errs, fmt.Errorf("the import %q must be related to the module name %q", m.Import, descriptor.ModuleName))
	}

	if m.DisplayName == "" {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing DisplayName", descriptor.ModuleName))
	}

	if m.Summary == "" {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing Summary", descriptor.ModuleName))
	}

	if m.TestData == nil {
		errs = multierror.Append(errs, fmt.Errorf("%s: missing TestData", descriptor.ModuleName))
	}

	return errs.ErrorOrNil()
}
