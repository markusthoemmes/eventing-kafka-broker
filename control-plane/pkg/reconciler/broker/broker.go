/*
 * Copyright 2020 The Knative Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package broker

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"

	"github.com/Shopify/sarama"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	eventing "knative.dev/eventing/pkg/apis/eventing/v1"
	"knative.dev/eventing/pkg/logging"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"

	coreconfig "knative.dev/eventing-kafka-broker/control-plane/pkg/core/config"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/log"
	"knative.dev/eventing-kafka-broker/control-plane/pkg/reconciler/base"
)

const (
	// topic prefix - (topic name: knative-broker-<broker-namespace>-<broker-name>)
	TopicPrefix = "knative-broker-"

	// signal that the broker hasn't been added to the config map yet.
	NoBroker = -1
)

type Reconciler struct {
	*base.Reconciler

	Resolver *resolver.URIResolver

	KafkaDefaultTopicDetails     sarama.TopicDetail
	KafkaDefaultTopicDetailsLock sync.RWMutex
	bootstrapServers             []string
	bootstrapServersLock         sync.RWMutex
	ConfigMapLister              corelisters.ConfigMapLister

	// NewClusterAdmin creates new sarama ClusterAdmin. It's convenient to add this as Reconciler field so that we can
	// mock the function used during the reconciliation loop.
	NewClusterAdmin func(addrs []string, config *sarama.Config) (sarama.ClusterAdmin, error)

	Configs *Configs
}

func (r *Reconciler) ReconcileKind(ctx context.Context, broker *eventing.Broker) reconciler.Event {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return r.reconcileKind(ctx, broker)
	})
}

func (r *Reconciler) reconcileKind(ctx context.Context, broker *eventing.Broker) reconciler.Event {

	logger := log.Logger(ctx, "reconcile", broker)

	statusConditionManager := statusConditionManager{
		Broker:   broker,
		configs:  r.Configs,
		recorder: controller.GetEventRecorder(ctx),
	}

	config, err := r.resolveBrokerConfig(logger, broker)
	if err != nil {
		return statusConditionManager.failedToResolveBrokerConfig(err)
	}
	statusConditionManager.brokerConfigResolved()

	logger.Debug("config resolved", zap.Any("config", config))

	topic, err := r.CreateTopic(logger, Topic(broker), config)
	if err != nil {
		return statusConditionManager.failedToCreateTopic(topic, err)
	}
	statusConditionManager.topicCreated(topic)

	logger.Debug("Topic created", zap.Any("topic", topic))

	// Get brokers and triggers config map.
	brokersTriggersConfigMap, err := r.GetOrCreateDataPlaneConfigMap()
	if err != nil {
		return statusConditionManager.failedToGetBrokersTriggersConfigMap(err)
	}

	logger.Debug("Got brokers and triggers config map")

	// Get brokersTriggers data.
	brokersTriggers, err := r.GetDataPlaneConfigMapData(logger, brokersTriggersConfigMap)
	if err != nil && brokersTriggers == nil {
		return statusConditionManager.failedToGetBrokersTriggersDataFromConfigMap(err)
	}

	logger.Debug(
		"Got brokers and triggers data from config map",
		zap.Any(base.BrokersTriggersDataLogKey, log.BrokersMarshaller{Brokers: brokersTriggers}),
	)

	brokerIndex := FindBroker(brokersTriggers, broker)

	// Get broker configuration.
	brokerConfig, err := r.getBrokerConfig(topic, broker, config)
	if err != nil {
		return statusConditionManager.failedToGetBrokerConfig(err)
	}
	// Update brokersTriggers data with the new broker configuration
	if brokerIndex != NoBroker {
		brokerConfig.Triggers = brokersTriggers.Brokers[brokerIndex].Triggers
		brokersTriggers.Brokers[brokerIndex] = brokerConfig

		logger.Debug("Broker exists", zap.Int("index", brokerIndex))

	} else {
		brokersTriggers.Brokers = append(brokersTriggers.Brokers, brokerConfig)

		logger.Debug("Broker doesn't exist")
	}

	// Increment volumeGeneration
	brokersTriggers.VolumeGeneration = incrementVolumeGeneration(brokersTriggers.VolumeGeneration)

	// Update the configuration map with the new brokersTriggers data.
	if err := r.UpdateDataPlaneConfigMap(brokersTriggers, brokersTriggersConfigMap); err != nil {
		return err
	}
	statusConditionManager.brokersTriggersConfigMapUpdated()

	logger.Debug("Brokers and triggers config map updated")

	// After #37 we reject events to a non-existing Broker, which means that we cannot consider a Broker Ready if all
	// receivers haven't got the Broker, so update failures to receiver pods is a hard failure.
	// On the other side, dispatcher pods care about Triggers, and the Broker object is used as a configuration
	// prototype for all associated Triggers, so we consider that it's fine on the dispatcher side to receive eventually
	// the update even if here eventually means seconds or minutes after the actual update.

	// Update volume generation annotation of receiver pods
	if err := r.UpdateReceiverPodsAnnotation(logger, brokersTriggers.VolumeGeneration); err != nil {
		return err
	}

	logger.Debug("Updated receiver pod annotation")

	// Update volume generation annotation of dispatcher pods
	if err := r.UpdateDispatcherPodsAnnotation(logger, brokersTriggers.VolumeGeneration); err != nil {
		// Failing to update dispatcher pods annotation leads to config map refresh delayed by several seconds.
		// Since the dispatcher side is the consumer side, we don't lose availability, and we can consider the Broker
		// ready. So, log out the error and move on to the next step.
		logger.Warn(
			"Failed to update dispatcher pod annotation to trigger an immediate config map refresh",
			zap.Error(err),
		)

		statusConditionManager.failedToUpdateDispatcherPodsAnnotation(err)
	} else {
		logger.Debug("Updated dispatcher pod annotation")
	}

	return statusConditionManager.reconciled()
}

func (r *Reconciler) FinalizeKind(ctx context.Context, broker *eventing.Broker) reconciler.Event {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return r.finalizeKind(ctx, broker)
	})
}

func (r *Reconciler) finalizeKind(ctx context.Context, broker *eventing.Broker) reconciler.Event {

	logger := log.Logger(ctx, "finalize", broker)

	// Get brokers and triggers config map.
	brokersTriggersConfigMap, err := r.GetOrCreateDataPlaneConfigMap()
	if err != nil {
		return fmt.Errorf("failed to get brokers and triggers config map %s: %w", r.Configs.DataPlaneConfigMapAsString(), err)
	}

	logger.Debug("Got brokers and triggers config map")

	// Get brokersTriggers data.
	brokersTriggers, err := r.GetDataPlaneConfigMapData(logger, brokersTriggersConfigMap)
	if err != nil {
		return fmt.Errorf("failed to get brokers and triggers: %w", err)
	}

	logger.Debug(
		"Got brokers and triggers data from config map",
		zap.Any(base.BrokersTriggersDataLogKey, log.BrokersMarshaller{Brokers: brokersTriggers}),
	)

	brokerIndex := FindBroker(brokersTriggers, broker)
	if brokerIndex != NoBroker {
		deleteBroker(brokersTriggers, brokerIndex)

		logger.Debug("Broker deleted", zap.Int("index", brokerIndex))

		// Update the configuration map with the new brokersTriggers data.
		if err := r.UpdateDataPlaneConfigMap(brokersTriggers, brokersTriggersConfigMap); err != nil {
			return err
		}

		logger.Debug("Brokers and triggers config map updated")

		// There is no need to update volume generation and dispatcher pod annotation, updates to the config map will
		// eventually be seen by the dispatcher pod and resources will be deleted accordingly.
	}

	config, err := r.resolveBrokerConfig(logger, broker)
	if err != nil {
		return fmt.Errorf("failed to resolve broker config: %w", err)
	}

	bootstrapServers := config.BootstrapServers
	topic, err := r.deleteTopic(Topic(broker), bootstrapServers)
	if err != nil {
		return fmt.Errorf("failed to delete topic %s: %w", topic, err)
	}

	logger.Debug("Topic deleted", zap.String("topic", topic))

	return nil
}

func incrementVolumeGeneration(generation uint64) uint64 {
	return (generation + 1) % (math.MaxUint64 - 1)
}

func (r *Reconciler) resolveBrokerConfig(logger *zap.Logger, broker *eventing.Broker) (*Config, error) {

	logger.Debug("broker config", zap.Any("broker.spec.config", broker.Spec.Config))

	if broker.Spec.Config == nil {
		return r.defaultConfig()
	}

	if strings.ToLower(broker.Spec.Config.Kind) != "configmap" { // TODO: is there any constant?
		return nil, fmt.Errorf("supported config Kind: ConfigMap - got %s", broker.Spec.Config.Kind)
	}

	namespace := broker.Spec.Config.Namespace
	if namespace == "" {
		// Namespace not specified, use broker namespace.
		namespace = broker.Namespace
	}
	cm, err := r.ConfigMapLister.ConfigMaps(namespace).Get(broker.Spec.Config.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get configmap %s/%s: %w", namespace, broker.Spec.Config.Name, err)
	}

	brokerConfig, err := configFromConfigMap(logger, cm)
	if err != nil {
		return nil, err
	}

	return brokerConfig, nil
}

func (r *Reconciler) defaultTopicDetail() sarama.TopicDetail {
	r.KafkaDefaultTopicDetailsLock.RLock()
	defer r.KafkaDefaultTopicDetailsLock.RUnlock()

	// copy the default topic details
	topicDetail := r.KafkaDefaultTopicDetails
	return topicDetail
}

func (r *Reconciler) defaultConfig() (*Config, error) {
	bootstrapServers, err := r.getDefaultBootstrapServersOrFail()
	if err != nil {
		return nil, err
	}

	return &Config{
		TopicDetail:      r.defaultTopicDetail(),
		BootstrapServers: bootstrapServers,
	}, nil
}

func (r *Reconciler) getBrokerConfig(topic string, broker *eventing.Broker, config *Config) (*coreconfig.Broker, error) {

	brokerConfig := &coreconfig.Broker{
		Id:               string(broker.UID),
		Topic:            topic,
		Path:             Path(broker.Namespace, broker.Name),
		BootstrapServers: config.getBootstrapServers(),
	}

	if broker.Spec.Delivery == nil || broker.Spec.Delivery.DeadLetterSink == nil {
		return brokerConfig, nil
	}

	deadLetterSinkURL, err := r.Resolver.URIFromDestinationV1(*broker.Spec.Delivery.DeadLetterSink, broker)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve broker.Spec.Deliver.DeadLetterSink: %w", err)
	}

	brokerConfig.DeadLetterSink = deadLetterSinkURL.String()

	return brokerConfig, nil
}

func (r *Reconciler) ConfigMapUpdated(ctx context.Context) func(configMap *corev1.ConfigMap) {

	return func(configMap *corev1.ConfigMap) {

		logger := logging.FromContext(ctx)

		config, err := configFromConfigMap(logger, configMap)
		if err != nil {
			return
		}

		logger.Debug("new defaults",
			zap.Any("topicDetail", config.TopicDetail),
			zap.String("BootstrapServers", config.getBootstrapServers()),
		)

		r.SetDefaultTopicDetails(config.TopicDetail)
		r.SetBootstrapServers(config.getBootstrapServers())
	}
}

// SetBootstrapServers change kafka bootstrap brokers addresses.
// servers: a comma separated list of brokers to connect to.
func (r *Reconciler) SetBootstrapServers(servers string) {
	if servers == "" {
		return
	}

	addrs := bootstrapServersArray(servers)

	r.bootstrapServersLock.Lock()
	r.bootstrapServers = addrs
	r.bootstrapServersLock.Unlock()
}

func (r *Reconciler) getKafkaClusterAdmin(bootstrapServers []string) (sarama.ClusterAdmin, error) {
	config := sarama.NewConfig()
	config.Version = sarama.MaxVersion

	kafkaClusterAdmin, err := r.NewClusterAdmin(bootstrapServers, config)
	if err != nil {
		return nil, fmt.Errorf("failed to create cluster admin: %w", err)
	}

	return kafkaClusterAdmin, nil
}

func (r *Reconciler) SetDefaultTopicDetails(topicDetail sarama.TopicDetail) {
	r.KafkaDefaultTopicDetailsLock.Lock()
	defer r.KafkaDefaultTopicDetailsLock.Unlock()

	r.KafkaDefaultTopicDetails = topicDetail
}

func FindBroker(brokersTriggers *coreconfig.Brokers, broker *eventing.Broker) int {
	// Find broker in brokersTriggers.
	brokerIndex := NoBroker
	for i, b := range brokersTriggers.Brokers {
		if b.Id == string(broker.UID) {
			brokerIndex = i
			break
		}
	}
	return brokerIndex
}

func deleteBroker(brokersTriggers *coreconfig.Brokers, index int) {
	if len(brokersTriggers.Brokers) == 1 {
		*brokersTriggers = coreconfig.Brokers{
			VolumeGeneration: brokersTriggers.VolumeGeneration,
		}
		return
	}

	// replace the broker to be deleted with the last one.
	brokersTriggers.Brokers[index] = brokersTriggers.Brokers[len(brokersTriggers.Brokers)-1]
	// truncate the array.
	brokersTriggers.Brokers = brokersTriggers.Brokers[:len(brokersTriggers.Brokers)-1]
}

func (r *Reconciler) getDefaultBootstrapServersOrFail() ([]string, error) {
	r.bootstrapServersLock.RLock()
	defer r.bootstrapServersLock.RUnlock()

	if len(r.bootstrapServers) == 0 {
		return nil, fmt.Errorf("no %s provided", BootstrapServersConfigMapKey)
	}

	return r.bootstrapServers, nil
}

func bootstrapServersArray(bootstrapServers string) []string {
	return strings.Split(bootstrapServers, ",")
}

func Path(namespace, name string) string {
	return fmt.Sprintf("/%s/%s", namespace, name)
}
