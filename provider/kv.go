// Package provider holds the different provider implementation.
package provider

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"strings"
	"text/template"
	"time"

	"errors"
	"github.com/BurntSushi/ty/fun"
	log "github.com/Sirupsen/logrus"
	"github.com/cenkalti/backoff"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/types"
	"github.com/docker/libkv"
	"github.com/docker/libkv/store"
)

// Kv holds common configurations of key-value providers.
type Kv struct {
	BaseProvider `mapstructure:",squash"`
	Endpoint     string
	Prefix       string
	TLS          *KvTLS
	storeType    store.Backend
	kvclient     store.Store
}

// KvTLS holds TLS specific configurations
type KvTLS struct {
	CA                 string
	Cert               string
	Key                string
	InsecureSkipVerify bool
}

// watchKv performs a watch on the given key prefix. Any time a change event happens on any keys matching
// the prefix, a new configuration is loaded and sent to configurationChan. This method will either
// return an error if the KV connection fails (after backoff/retry) or run perpetually until a stop message
// is sent on the stop chan.
func (provider *Kv) watchKv(configurationChan chan<- types.ConfigMessage, prefix string, stop chan bool) error {
	var events <-chan []*store.KVPair

	for {
		select {
		case <-stop:
			return nil
		default: // noop
		}

		operation := func() error {
			if events == nil {
				var err error
				events, err = provider.kvclient.WatchTree(provider.Prefix, make(chan struct{}))
				if err != nil {
					return err
				}
			}

			// Reading from the events chan may block indefinitely if there are no events,
			// however, the operation must return relatively quickly in order for backoff.RetryNotify
			// to work as expected. Here, we add a timeout to protect operation from running too long.
			timeout := make(chan bool, 1)
			go func() {
				time.Sleep(1 * time.Second)
				timeout <- true
			}()

			select {
			case <-timeout:
				return nil
			case _, ok := <-events:
				if !ok {
					events = nil
					return errors.New("watchtree channel closed")
				}
				configuration := provider.loadConfig()
				if configuration != nil {
					configurationChan <- types.ConfigMessage{
						ProviderName:  string(provider.storeType),
						Configuration: configuration,
					}
				}
			}
			return nil
		}

		notify := func(err error, time time.Duration) {
			log.Errorf("KV watch connection error: %+v, retrying in %s", err, time)
		}
		b := backoff.NewExponentialBackOff()
		b.MaxElapsedTime = 0 // Keep backing off forever
		err := backoff.RetryNotify(operation, b, notify)
		if err != nil {
			return fmt.Errorf("Cannot connect to KV server: %v", err)
		}
	}
}

func (provider *Kv) provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool) error {
	storeConfig := &store.Config{
		ConnectionTimeout: 30 * time.Second,
		Bucket:            "traefik",
	}

	if provider.TLS != nil {
		caPool := x509.NewCertPool()

		if provider.TLS.CA != "" {
			ca, err := ioutil.ReadFile(provider.TLS.CA)

			if err != nil {
				return fmt.Errorf("Failed to read CA. %s", err)
			}

			caPool.AppendCertsFromPEM(ca)
		}

		cert, err := tls.LoadX509KeyPair(provider.TLS.Cert, provider.TLS.Key)

		if err != nil {
			return fmt.Errorf("Failed to load keypair. %s", err)
		}

		storeConfig.TLS = &tls.Config{
			Certificates:       []tls.Certificate{cert},
			RootCAs:            caPool,
			InsecureSkipVerify: provider.TLS.InsecureSkipVerify,
		}
	}

	operation := func() error {
		kv, err := libkv.NewStore(
			provider.storeType,
			strings.Split(provider.Endpoint, ","),
			storeConfig,
		)
		if err != nil {
			return fmt.Errorf("Failed to Connect to KV store: %v", err)
		}
		if _, err := kv.Exists("qmslkjdfmqlskdjfmqlksjazçueznbvbwzlkajzebvkwjdcqmlsfj"); err != nil {
			return fmt.Errorf("Failed to test KV store connection: %v", err)
		}
		provider.kvclient = kv
		if provider.Watch {
			pool.Go(func(stop chan bool) {
				err := provider.watchKv(configurationChan, provider.Prefix, stop)
				if err != nil {
					log.Errorf("Cannot watch KV store: %v", err)
				}
			})
		}
		configuration := provider.loadConfig()
		configurationChan <- types.ConfigMessage{
			ProviderName:  string(provider.storeType),
			Configuration: configuration,
		}
		return nil
	}
	notify := func(err error, time time.Duration) {
		log.Errorf("KV connection error: %+v, retrying in %s", err, time)
	}
	err := backoff.RetryNotify(operation, backoff.NewExponentialBackOff(), notify)
	if err != nil {
		return fmt.Errorf("Cannot connect to KV server: %v", err)
	}
	return nil
}

func (provider *Kv) loadConfig() *types.Configuration {
	templateObjects := struct {
		Prefix string
	}{
		// Allow `/traefik/alias` to superesede `provider.Prefix`
		strings.TrimSuffix(provider.get(provider.Prefix, provider.Prefix+"/alias"), "/"),
	}
	var KvFuncMap = template.FuncMap{
		"List":     provider.list,
		"Get":      provider.get,
		"SplitGet": provider.splitGet,
		"Last":     provider.last,
	}

	configuration, err := provider.getConfiguration("templates/kv.tmpl", KvFuncMap, templateObjects)
	if err != nil {
		log.Error(err)
	}
	return configuration
}

func (provider *Kv) list(keys ...string) []string {
	joinedKeys := strings.Join(keys, "")
	keysPairs, err := provider.kvclient.List(joinedKeys)
	if err != nil {
		log.Errorf("Error getting keys %s %s ", joinedKeys, err)
		return nil
	}
	directoryKeys := make(map[string]string)
	for _, key := range keysPairs {
		directory := strings.Split(strings.TrimPrefix(key.Key, joinedKeys), "/")[0]
		directoryKeys[directory] = joinedKeys + directory
	}
	return fun.Values(directoryKeys).([]string)
}

func (provider *Kv) get(defaultValue string, keys ...string) string {
	joinedKeys := strings.Join(keys, "")
	keyPair, err := provider.kvclient.Get(strings.TrimPrefix(joinedKeys, "/"))
	if err != nil {
		log.Warnf("Error getting key %s %s, setting default %s", joinedKeys, err, defaultValue)
		return defaultValue
	} else if keyPair == nil {
		log.Warnf("Error getting key %s, setting default %s", joinedKeys, defaultValue)
		return defaultValue
	}
	return string(keyPair.Value)
}

func (provider *Kv) splitGet(keys ...string) []string {
	joinedKeys := strings.Join(keys, "")
	keyPair, err := provider.kvclient.Get(joinedKeys)
	if err != nil {
		log.Warnf("Error getting key %s %s, setting default empty", joinedKeys, err)
		return []string{}
	} else if keyPair == nil {
		log.Warnf("Error getting key %s, setting default %empty", joinedKeys)
		return []string{}
	}
	return strings.Split(string(keyPair.Value), ",")
}

func (provider *Kv) last(key string) string {
	splittedKey := strings.Split(key, "/")
	return splittedKey[len(splittedKey)-1]
}
