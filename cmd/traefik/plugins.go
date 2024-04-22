package main

import (
	"fmt"

	"github.com/traefik/traefik/v2/pkg/config/static"
	"github.com/traefik/traefik/v2/pkg/plugins"
)

const outputDir = "./plugins-storage/"

func createPluginBuilder(
	staticConfiguration *static.Configuration, // 静态配置
) (*plugins.Builder, error) {
	// 加载远端插件和本地插件，如果是远端插件就从Traefik插件仓库中下载，如果本地插件直接读取本地插件的配置文件然后加载
	client, plgs, localPlgs, err := initPlugins(staticConfiguration)
	if err != nil {
		return nil, err
	}

	// 记载本地插件和远端插件到内存中
	return plugins.NewBuilder(client, plgs, localPlgs)
}

// 加载远端插件和本地插件，如果是远端插件就从Traefik插件仓库中下载，如果本地插件直接读取本地插件的配置文件然后加载
func initPlugins(staticCfg *static.Configuration /*静态配置*/) (
	*plugins.Client, // 远端插件的HTTP下载客户端
	map[string]plugins.Descriptor, // 远端插件
	map[string]plugins.LocalDescriptor, // 本地插件
	error) {
	// 校验插件的名字必须唯一
	err := checkUniquePluginNames(staticCfg.Experimental)
	if err != nil {
		return nil, nil, nil, err
	}

	var client *plugins.Client
	plgs := map[string]plugins.Descriptor{}

	// 判断用户是否配置了远端插件
	if hasPlugins(staticCfg) {
		opts := plugins.ClientOptions{
			Output: outputDir, // ./plugins-storage/ 目录
		}

		var err error
		// 创建一个客户端，用于从仓库中下载Traefik的插件
		client, err = plugins.NewClient(opts)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to create plugins client: %w", err)
		}

		// 下载用户配置的所有插件，然后解压缩到指定的目录
		err = plugins.SetupRemotePlugins(client, staticCfg.Experimental.Plugins)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("unable to set up plugins environment: %w", err)
		}

		plgs = staticCfg.Experimental.Plugins
	}

	localPlgs := map[string]plugins.LocalDescriptor{}

	// 判断用户是否配置了本地插件
	if hasLocalPlugins(staticCfg) {
		// 加载本地插件
		err := plugins.SetupLocalPlugins(staticCfg.Experimental.LocalPlugins)
		if err != nil {
			return nil, nil, nil, err
		}

		localPlgs = staticCfg.Experimental.LocalPlugins
	}

	return client, plgs, localPlgs, nil
}

func checkUniquePluginNames(e *static.Experimental) error {
	if e == nil {
		return nil
	}

	for s := range e.LocalPlugins {
		if _, ok := e.Plugins[s]; ok {
			return fmt.Errorf("the plugin's name %q must be unique", s)
		}
	}

	return nil
}

// 如果用户配置了远端插件，则返回true
func hasPlugins(staticCfg *static.Configuration) bool {
	return staticCfg.Experimental != nil && len(staticCfg.Experimental.Plugins) > 0
}

// 判断用户是否配置了本地插件
func hasLocalPlugins(staticCfg *static.Configuration) bool {
	return staticCfg.Experimental != nil && len(staticCfg.Experimental.LocalPlugins) > 0
}
