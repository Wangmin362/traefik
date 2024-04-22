package static

import "github.com/traefik/traefik/v2/pkg/plugins"

// Experimental experimental Traefik features.
type Experimental struct {
	// TODO 插件和本地插件的有何区别？  这里的插件应该就是需要通过URL下载的插件，而本地插件应该是本地已经存在的插件
	Plugins map[string]plugins.Descriptor `description:"Plugins configuration." json:"plugins,omitempty" toml:"plugins,omitempty" yaml:"plugins,omitempty" export:"true"`
	// 什么是本地插件？
	LocalPlugins map[string]plugins.LocalDescriptor `description:"Local plugins configuration." json:"localPlugins,omitempty" toml:"localPlugins,omitempty" yaml:"localPlugins,omitempty" export:"true"`

	KubernetesGateway bool `description:"Allow the Kubernetes gateway api provider usage." json:"kubernetesGateway,omitempty" toml:"kubernetesGateway,omitempty" yaml:"kubernetesGateway,omitempty" export:"true"`
	HTTP3             bool `description:"Enable HTTP3." json:"http3,omitempty" toml:"http3,omitempty" yaml:"http3,omitempty" export:"true"`
}
