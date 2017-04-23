// Copyright 2016 The nats-operator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/fakod/nats-operator/pkg/cluster"
	"github.com/fakod/nats-operator/pkg/spec"
	"github.com/fakod/nats-operator/pkg/util/k8sutil"

	"github.com/Sirupsen/logrus"
	k8sapi "k8s.io/kubernetes/pkg/api"
	unversionedAPI "k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/unversioned"
)

const (
	tprName = "management.nats.io"
)

var (
	supportedPVProvisioners = map[string]struct{}{
		"kubernetes.io/gce-pd":  {},
		"kubernetes.io/aws-ebs": {},
	}

	ErrVersionOutdated = errors.New("Requested version is outdated.")

	initRetryWaitTime = 30 * time.Second
)

type rawEvent struct {
	Type   string
	Object json.RawMessage
}

type Event struct {
	Type   string
	Object *spec.NatsCluster
}

type Controller struct {
	logger *logrus.Entry

	Config
	clusters    map[string]*cluster.Cluster
	stopChMap   map[string]chan struct{}
	waitCluster sync.WaitGroup
}

type Config struct {
	Namespace     string
	MasterHost    string
	KubeCli       *unversioned.Client
	PVProvisioner string
}

func (c *Config) validate() error {
	if _, ok := supportedPVProvisioners[c.PVProvisioner]; !ok {
		return fmt.Errorf(
			"persistent volume provisioner %s is not supported: options = %v",
			c.PVProvisioner, supportedPVProvisioners,
		)
	}
	return nil
}

func New(cfg Config) *Controller {
	if err := cfg.validate(); err != nil {
		panic(err)
	}
	return &Controller{
		logger: logrus.WithField("pkg", "controller"),

		Config:    cfg,
		clusters:  make(map[string]*cluster.Cluster),
		stopChMap: map[string]chan struct{}{},
	}
}

func (c *Controller) Run() error {
	var (
		watchVersion string
		err          error
	)

	for {
		watchVersion, err = c.initResource()
		if err == nil {
			break
		}
		c.logger.Errorf("NATS operator initialization failed: %v", err)
		c.logger.Infof("Retrying in %v...", initRetryWaitTime)
		time.Sleep(initRetryWaitTime)
		// TODO: add max retry?
	}

	defer func() {
		for _, stopC := range c.stopChMap {
			close(stopC)
		}
		c.waitCluster.Wait()
	}()

	eventCh, errCh := c.monitor(watchVersion)

	go func() {
		for event := range eventCh {
			clusterName := event.Object.ObjectMeta.Name
			switch event.Type {
			case "ADDED":
				clusterSpec := &event.Object.Spec

				stopC := make(chan struct{})
				c.stopChMap[clusterName] = stopC

				nc := cluster.New(c.KubeCli, clusterName, c.Namespace, clusterSpec, stopC, &c.waitCluster)
				c.clusters[clusterName] = nc
			case "MODIFIED":
				if c.clusters[clusterName] == nil {
					c.logger.Warningf("Ignoring modification event: cluster %q not found (or dead)", clusterName)
					break
				}
				c.clusters[clusterName].Update(&event.Object.Spec)
			case "DELETED":
				if c.clusters[clusterName] == nil {
					c.logger.Warningf("Ignoring deletion event: cluster %q not found (or dead)", clusterName)
					break
				}
				c.clusters[clusterName].Delete()
				delete(c.clusters, clusterName)
			}
		}
	}()
	return <-errCh
}

func (c *Controller) findAllClusters() (string, error) {
	c.logger.Info("Retrieving existing NATS clusters...")
	resp, err := k8sutil.ListClusters(c.MasterHost, c.Namespace, c.KubeCli.RESTClient.Client)
	if err != nil {
		return "", err
	}
	d := json.NewDecoder(resp.Body)
	list := &NATSClusterList{}
	if err := d.Decode(list); err != nil {
		return "", err
	}
	for _, item := range list.Items {
		stopC := make(chan struct{})
		c.stopChMap[item.Name] = stopC

		nc := cluster.Restore(c.KubeCli, item.Name, c.Namespace, &item.Spec, stopC, &c.waitCluster)
		c.clusters[item.Name] = nc
	}
	return list.ListMeta.ResourceVersion, nil
}

func (c *Controller) initResource() (string, error) {
	watchVersion := "0"
	err := c.createTPR()
	if err != nil {
		if k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			watchVersion, err = c.findAllClusters()
			if err != nil {
				return "", err
			}
		} else {
			return "", fmt.Errorf("Failed to create TPR: %v", err)
		}
	}

	// TODO use for streaming
	//err = k8sutil.CreateStorageClass(c.KubeCli, c.PVProvisioner)
	//if err != nil {
	//	if !k8sutil.IsKubernetesResourceAlreadyExistError(err) {
	//		return "", fmt.Errorf("fail to create storage class: %v", err)
	//	}
	//}
	return watchVersion, nil
}

func (c *Controller) createTPR() error {
	tpr := &extensions.ThirdPartyResource{
		ObjectMeta: k8sapi.ObjectMeta{
			Name: tprName,
		},
		Versions: []extensions.APIVersion{
			{Name: "v1"},
		},
		Description: "Manage NATS clusters",
	}
	_, err := c.KubeCli.ThirdPartyResources().Create(tpr)
	if err != nil {
		return err
	}

	return k8sutil.WaitTPRReady(c.KubeCli.Client, 3*time.Second, 30*time.Second, c.MasterHost, c.Namespace)
}

func (c *Controller) monitor(watchVersion string) (<-chan *Event, <-chan error) {
	host := c.MasterHost
	ns := c.Namespace
	httpClient := c.KubeCli.Client

	eventCh := make(chan *Event)
	// On unexpected error case, controller should exit
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)

		for {
			resp, err := k8sutil.WatchClusters(host, ns, httpClient, watchVersion)
			if err != nil {
				errCh <- err
				return
			}
			if resp.StatusCode != 200 {
				resp.Body.Close()
				errCh <- errors.New("Invalid status code: " + resp.Status)
				return
			}

			decoder := json.NewDecoder(resp.Body)
			for {
				ev, st, err := pollEvent(decoder)

				if err != nil {
					if err == io.EOF { // apiserver will close stream periodically
						c.logger.Debug("API server closed stream")
						break
					}

					c.logger.Errorf("Received invalid event from API server: %v", err)
					errCh <- err
					return
				}

				if st != nil {
					if st.Code == http.StatusGone { // event history is outdated
						errCh <- ErrVersionOutdated // go to recovery path
						return
					}
					c.logger.Fatalf("Unexpected status response from API server: %v", st.Message)
				}

				c.logger.Debugf("NATS cluster event: %v %v", ev.Type, ev.Object.Spec)

				watchVersion = ev.Object.ObjectMeta.ResourceVersion
				eventCh <- ev
			}

			resp.Body.Close()
		}
	}()

	return eventCh, errCh
}

func pollEvent(decoder *json.Decoder) (*Event, *unversionedAPI.Status, error) {
	re := &rawEvent{}
	err := decoder.Decode(re)
	if err != nil {
		if err == io.EOF {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("Failed to decode raw event: %+v", err)
	}

	if re.Type == "ERROR" {
		status := &unversionedAPI.Status{}
		err = json.Unmarshal(re.Object, status)
		if err != nil {
			return nil, nil, fmt.Errorf("Failed to decode %+v into unversioned.Status %+v", re.Object, err)
		}
		return nil, status, nil
	}

	ev := &Event{
		Type:   re.Type,
		Object: &spec.NatsCluster{},
	}
	err = json.Unmarshal(re.Object, ev.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("Failed to unmarshal NATSCluster object from data %+v: %+v", re.Object, err)
	}
	return ev, nil, nil
}
