package provider

import (
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/safe"
)

// Provider defines methods of a provider.
// TODO 如何理解Traefik Provider的抽象？
// 1、Provider其实就是向Traefik提供动态配置的组件。
// 2、在不同的场景中，不同的Provider只需要根据特定的场景监听感兴趣的数据，与此同时生成合适的动态配置，通过Provide接口把配置交给Traefik结论。
// 对于Docker来说，只需要监听Docker Event,看看当前的Event所对应的容器是否是用户关心的容器，如果是那么就生成合适的配置。对于K8S平台来说，
// Provider需要通过WATCH API，监听Deployment或者Pod的变化，然后生成合适的配置即可。
type Provider interface {
	// Provide allows the provider to provide configurations to traefik
	// using the given configuration channel.
	Provide(configurationChan chan<- dynamic.Message, pool *safe.Pool) error
	Init() error
}
