package dynamic

import (
	"github.com/traefik/traefik/v2/pkg/tls"
)

// +k8s:deepcopy-gen=true

// Message holds configuration information exchanged between parts of traefik.
type Message struct {
	ProviderName  string
	Configuration *Configuration
}

// +k8s:deepcopy-gen=true

// Configurations is for currentConfigurations Map.
type Configurations map[string]*Configuration

// +k8s:deepcopy-gen=true

// Configuration is the root of the dynamic configuration.
// Traefik的动态配置
type Configuration struct {
	HTTP *HTTPConfiguration `json:"http,omitempty" toml:"http,omitempty" yaml:"http,omitempty" export:"true"`
	TCP  *TCPConfiguration  `json:"tcp,omitempty" toml:"tcp,omitempty" yaml:"tcp,omitempty" export:"true"`
	UDP  *UDPConfiguration  `json:"udp,omitempty" toml:"udp,omitempty" yaml:"udp,omitempty" export:"true"`
	// 1、和路由、后端服务一样，Traefik也需要动态管理证书。因为用户很有可能会给每个可能的路由都配置一个合适的证书，因此Traefik需要动态发现不同
	// 的证书
	// 2、为什么HTTP路由当中不用设置使用哪套证书呢？其实原因很简单，因为证书本身就是和域名绑定的。每一个证书都绑定了特定的域名，所以HTTP路由
	// 只需要声明启用HTTP路由即可，那么Traefik就会自动根据客户端请求的域名取拿到合适的证书。
	TLS *TLSConfiguration `json:"tls,omitempty" toml:"tls,omitempty" yaml:"tls,omitempty" export:"true"`
}

// +k8s:deepcopy-gen=true

// TLSConfiguration contains all the configuration parameters of a TLS connection.
type TLSConfiguration struct {
	// 1、用于配置不同的证书
	Certificates []*tls.CertAndStores `json:"certificates,omitempty"  toml:"certificates,omitempty" yaml:"certificates,omitempty" label:"-" export:"true"`
	// 1、用于配置不同TLS的配置参数，可以做TLS更精细化的配置，譬如指定Cipher套件版本呢，TLS版本等等
	// 2、这里的key其实和HTTP路由中的TLS.Options是相对应的，我们可以在这里配置不同的TLS参数，然后在HTTP路由中的TLS.Options设置相同的key即可
	Options map[string]tls.Options `json:"options,omitempty" toml:"options,omitempty" yaml:"options,omitempty" label:"-" export:"true"`
	// 1、TODO 暂时没有看懂Traefik证书存储这一块的设计
	Stores map[string]tls.Store `json:"stores,omitempty" toml:"stores,omitempty" yaml:"stores,omitempty" export:"true"`
}
