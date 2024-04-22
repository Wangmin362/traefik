package plugins

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/yaegi/interp"
	"github.com/traefik/yaegi/stdlib"
)

// Constructor creates a plugin handler.
type Constructor func(context.Context, http.Handler) (http.Handler, error)

// Builder is a plugin builder.
type Builder struct {
	middlewareBuilders map[string]*middlewareBuilder
	providerBuilders   map[string]providerBuilder
}

// NewBuilder creates a new Builder.
func NewBuilder(
	client *Client, // 远端插件的HTTP下载客户按
	plugins map[string]Descriptor, // 远端插件
	localPlugins map[string]LocalDescriptor, // 本地插件
) (*Builder, error) {
	pb := &Builder{
		middlewareBuilders: map[string]*middlewareBuilder{},
		providerBuilders:   map[string]providerBuilder{},
	}

	for pName, desc := range plugins { // 遍历远端插件
		// 读取插件的配置
		manifest, err := client.ReadManifest(desc.ModuleName)
		if err != nil {
			_ = client.ResetAll()
			return nil, fmt.Errorf("%s: failed to read manifest: %w", desc.ModuleName, err)
		}

		logger := log.WithoutContext().WithFields(logrus.Fields{"plugin": "plugin-" + pName, "module": desc.ModuleName})
		// TODO 加载插件
		i := interp.New(interp.Options{
			GoPath: client.GoPath(),
			Env:    os.Environ(),
			Stdout: logger.WriterLevel(logrus.DebugLevel),
			Stderr: logger.WriterLevel(logrus.ErrorLevel),
		})

		err = i.Use(stdlib.Symbols)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to load symbols: %w", desc.ModuleName, err)
		}

		err = i.Use(ppSymbols())
		if err != nil {
			return nil, fmt.Errorf("%s: failed to load provider symbols: %w", desc.ModuleName, err)
		}

		_, err = i.Eval(fmt.Sprintf(`import "%s"`, manifest.Import))
		if err != nil {
			return nil, fmt.Errorf("%s: failed to import plugin code %q: %w", desc.ModuleName, manifest.Import, err)
		}

		switch manifest.Type {
		case "middleware":
			middleware, err := newMiddlewareBuilder(i, manifest.BasePkg, manifest.Import)
			if err != nil {
				return nil, err
			}

			pb.middlewareBuilders[pName] = middleware
		case "provider":
			pb.providerBuilders[pName] = providerBuilder{
				interpreter: i,
				Import:      manifest.Import,
				BasePkg:     manifest.BasePkg,
			}
		default:
			return nil, fmt.Errorf("unknow plugin type: %s", manifest.Type)
		}
	}

	// 加载本地插件
	for pName, desc := range localPlugins {
		manifest, err := ReadManifest(localGoPath, desc.ModuleName)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to read manifest: %w", desc.ModuleName, err)
		}

		logger := log.WithoutContext().WithFields(logrus.Fields{"plugin": "plugin-" + pName, "module": desc.ModuleName})
		// 加载本地插件
		i := interp.New(interp.Options{
			GoPath: localGoPath,
			Env:    os.Environ(),
			Stdout: logger.WriterLevel(logrus.DebugLevel),
			Stderr: logger.WriterLevel(logrus.ErrorLevel),
		})

		err = i.Use(stdlib.Symbols)
		if err != nil {
			return nil, fmt.Errorf("%s: failed to load symbols: %w", desc.ModuleName, err)
		}

		err = i.Use(ppSymbols())
		if err != nil {
			return nil, fmt.Errorf("%s: failed to load provider symbols: %w", desc.ModuleName, err)
		}

		_, err = i.Eval(fmt.Sprintf(`import "%s"`, manifest.Import))
		if err != nil {
			return nil, fmt.Errorf("%s: failed to import plugin code %q: %w", desc.ModuleName, manifest.Import, err)
		}

		switch manifest.Type {
		case "middleware":
			middleware, err := newMiddlewareBuilder(i, manifest.BasePkg, manifest.Import)
			if err != nil {
				return nil, err
			}

			pb.middlewareBuilders[pName] = middleware
		case "provider":
			pb.providerBuilders[pName] = providerBuilder{
				interpreter: i,
				Import:      manifest.Import,
				BasePkg:     manifest.BasePkg,
			}
		default:
			return nil, fmt.Errorf("unknow plugin type: %s", manifest.Type)
		}
	}

	return pb, nil
}
