package server

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/sirupsen/logrus"
	"github.com/traefik/traefik/v2/pkg/config/dynamic"
	"github.com/traefik/traefik/v2/pkg/log"
	"github.com/traefik/traefik/v2/pkg/provider"
	"github.com/traefik/traefik/v2/pkg/safe"
	"github.com/traefik/traefik/v2/pkg/tls"
)

// ConfigurationWatcher watches configuration changes.
type ConfigurationWatcher struct {
	// 本质上providerAggregator就是一个Provider数组，当需要Provider时，会把活委派给内部的Provider
	providerAggregator provider.Provider

	// TODO 默认的入口点有什么用？看起来默认的入口点其实就是除了UDP入口点以及Traefik内部入口点以外的所有TCP入口点
	defaultEntryPoints []string

	// 动态发现消息。docker或者k8s可能会动态增加容器、删减容器
	allProvidersConfigs chan dynamic.Message

	// 动态配置，这个动态配置极有可能会发生更改
	newConfigs chan dynamic.Configurations

	requiredProvider string
	// 用于处理动态修改的配置
	configurationListeners []func(dynamic.Configuration)

	routinesPool *safe.Pool
}

// NewConfigurationWatcher creates a new ConfigurationWatcher.
func NewConfigurationWatcher(
	routinesPool *safe.Pool,
	pvd provider.Provider,
	defaultEntryPoints []string,
	requiredProvider string, // 其实就是Traefik internal Provider
) *ConfigurationWatcher {
	return &ConfigurationWatcher{
		providerAggregator:  pvd,
		allProvidersConfigs: make(chan dynamic.Message, 100),
		newConfigs:          make(chan dynamic.Configurations),
		routinesPool:        routinesPool,
		defaultEntryPoints:  defaultEntryPoints,
		requiredProvider:    requiredProvider,
	}
}

// Start the configuration watcher.
func (c *ConfigurationWatcher) Start() {
	c.routinesPool.GoCtx(c.receiveConfigurations)
	c.routinesPool.GoCtx(c.applyConfigurations)
	c.startProviderAggregator()
}

// Stop the configuration watcher.
func (c *ConfigurationWatcher) Stop() {
	close(c.allProvidersConfigs)
	close(c.newConfigs)
}

// AddListener adds a new listener function used when new configuration is provided.
// 添加Listener，当动态配置发生变更时，会调用所有的Listener
func (c *ConfigurationWatcher) AddListener(listener func(dynamic.Configuration)) {
	if c.configurationListeners == nil {
		c.configurationListeners = make([]func(dynamic.Configuration), 0)
	}
	c.configurationListeners = append(c.configurationListeners, listener)
}

func (c *ConfigurationWatcher) startProviderAggregator() {
	logger := log.WithoutContext()

	logger.Infof("Starting provider aggregator %T", c.providerAggregator)

	safe.Go(func() {
		err := c.providerAggregator.Provide(c.allProvidersConfigs, c.routinesPool)
		if err != nil {
			logger.Errorf("Error starting provider aggregator %T: %s", c.providerAggregator, err)
		}
	})
}

// receiveConfigurations receives configuration changes from the providers.
// The configuration message then gets passed along a series of check, notably
// to verify that, for a given provider, the configuration that was just received
// is at least different from the previously received one.
// The full set of configurations is then sent to the throttling goroutine,
// (throttleAndApplyConfigurations) via a RingChannel, which ensures that we can
// constantly send in a non-blocking way to the throttling goroutine the last
// global state we are aware of.
func (c *ConfigurationWatcher) receiveConfigurations(ctx context.Context) {
	newConfigurations := make(dynamic.Configurations)
	var output chan dynamic.Configurations
	for {
		select {
		case <-ctx.Done():
			return
		// DeepCopy is necessary because newConfigurations gets modified later by the consumer of c.newConfigs
		case output <- newConfigurations.DeepCopy():
			output = nil

		default:
			select {
			case <-ctx.Done():
				return
			case configMsg, ok := <-c.allProvidersConfigs:
				if !ok {
					return
				}

				logger := log.WithoutContext().WithField(log.ProviderName, configMsg.ProviderName)

				if configMsg.Configuration == nil {
					logger.Debug("Skipping nil configuration.")
					continue
				}

				if isEmptyConfiguration(configMsg.Configuration) {
					logger.Debug("Skipping empty configuration.")
					continue
				}

				logConfiguration(logger, configMsg)

				if reflect.DeepEqual(newConfigurations[configMsg.ProviderName], configMsg.Configuration) {
					// no change, do nothing
					logger.Debug("Skipping unchanged configuration.")
					continue
				}

				newConfigurations[configMsg.ProviderName] = configMsg.Configuration.DeepCopy()

				output = c.newConfigs

			// DeepCopy is necessary because newConfigurations gets modified later by the consumer of c.newConfigs
			case output <- newConfigurations.DeepCopy():
				output = nil
			}
		}
	}
}

// applyConfigurations blocks on a RingChannel that receives the new
// set of configurations that is compiled and sent by receiveConfigurations as soon
// as a provider change occurs. If the new set is different from the previous set
// that had been applied, the new set is applied, and we sleep for a while before
// listening on the channel again.
func (c *ConfigurationWatcher) applyConfigurations(ctx context.Context) {
	var lastConfigurations dynamic.Configurations
	for {
		select {
		case <-ctx.Done():
			return
		case newConfigs, ok := <-c.newConfigs:
			if !ok {
				return
			}

			// We wait for first configuration of the required provider before applying configurations.
			if _, ok := newConfigs[c.requiredProvider]; c.requiredProvider != "" && !ok {
				continue
			}

			if reflect.DeepEqual(newConfigs, lastConfigurations) {
				continue
			}

			conf := mergeConfiguration(newConfigs.DeepCopy(), c.defaultEntryPoints)
			conf = applyModel(conf)

			for _, listener := range c.configurationListeners {
				listener(conf)
			}

			lastConfigurations = newConfigs
		}
	}
}

func logConfiguration(logger log.Logger, configMsg dynamic.Message) {
	if log.GetLevel() != logrus.DebugLevel {
		return
	}

	copyConf := configMsg.Configuration.DeepCopy()
	if copyConf.TLS != nil {
		copyConf.TLS.Certificates = nil

		if copyConf.TLS.Options != nil {
			cleanedOptions := make(map[string]tls.Options, len(copyConf.TLS.Options))
			for name, option := range copyConf.TLS.Options {
				option.ClientAuth.CAFiles = []tls.FileOrContent{}
				cleanedOptions[name] = option
			}

			copyConf.TLS.Options = cleanedOptions
		}

		for k := range copyConf.TLS.Stores {
			st := copyConf.TLS.Stores[k]
			st.DefaultCertificate = nil
			copyConf.TLS.Stores[k] = st
		}
	}

	if copyConf.HTTP != nil {
		for _, transport := range copyConf.HTTP.ServersTransports {
			transport.Certificates = tls.Certificates{}
			transport.RootCAs = []tls.FileOrContent{}
		}
	}

	jsonConf, err := json.Marshal(copyConf)
	if err != nil {
		logger.Errorf("Could not marshal dynamic configuration: %v", err)
		logger.Debugf("Configuration received: [struct] %#v", copyConf)
	} else {
		logger.Debugf("Configuration received: %s", string(jsonConf))
	}
}

func isEmptyConfiguration(conf *dynamic.Configuration) bool {
	if conf.TCP == nil {
		conf.TCP = &dynamic.TCPConfiguration{}
	}
	if conf.HTTP == nil {
		conf.HTTP = &dynamic.HTTPConfiguration{}
	}
	if conf.UDP == nil {
		conf.UDP = &dynamic.UDPConfiguration{}
	}

	httpEmpty := conf.HTTP.Routers == nil && conf.HTTP.Services == nil && conf.HTTP.Middlewares == nil
	tlsEmpty := conf.TLS == nil || conf.TLS.Certificates == nil && conf.TLS.Stores == nil && conf.TLS.Options == nil
	tcpEmpty := conf.TCP.Routers == nil && conf.TCP.Services == nil && conf.TCP.Middlewares == nil
	udpEmpty := conf.UDP.Routers == nil && conf.UDP.Services == nil

	return httpEmpty && tlsEmpty && tcpEmpty && udpEmpty
}
