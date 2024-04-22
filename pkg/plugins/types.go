package plugins

// Descriptor The static part of a plugin configuration.
type Descriptor struct {
	// ModuleName (required)
	ModuleName string `description:"plugin's module name." json:"moduleName,omitempty" toml:"moduleName,omitempty" yaml:"moduleName,omitempty" export:"true"`

	// Version (required)
	Version string `description:"plugin's version." json:"version,omitempty" toml:"version,omitempty" yaml:"version,omitempty" export:"true"`
}

// LocalDescriptor The static part of a local plugin configuration.
// Q:本地插件的插件目录应该放在哪里？ A: 本地插件的插件目录应该放在 ./plugins-local/src/<module_name>目录下
// 每个本地插件都应该放在 ./plugins-local/src/<module_name>目录下,同时应该存在一个.traefik.yml文件，文件的全路径为 ./plugins-local/src/<module_name>/.traefik.yml
type LocalDescriptor struct {
	// ModuleName (required)
	ModuleName string `description:"plugin's module name." json:"moduleName,omitempty" toml:"moduleName,omitempty" yaml:"moduleName,omitempty" export:"true"`
}

// Manifest The plugin manifest.
// 每个插件的配置文件格式如下：
type Manifest struct {
	DisplayName   string                 `yaml:"displayName"`   // TODO ?
	Type          string                 `yaml:"type"`          // 目前支持的类型只有 middleware 和 provider
	Import        string                 `yaml:"import"`        // TODO ?
	BasePkg       string                 `yaml:"basePkg"`       // TODO ?
	Compatibility string                 `yaml:"compatibility"` // TODO ?
	Summary       string                 `yaml:"summary"`       // TODO ?
	TestData      map[string]interface{} `yaml:"testData"`
}
