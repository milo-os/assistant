// Package apiserver assembles the conversations aggregated API server: the
// runtime scheme/codecs for the assistant group and the generic apiserver
// wiring that installs the bespoke Conversation REST (a read view over the
// shared history store) — no etcd, no generic registry.
package apiserver

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/klog/v2"

	"github.com/milo-os/assistant/internal/apiserver/registry/capabilitygapreport"
	"github.com/milo-os/assistant/internal/apiserver/registry/conversation"
	"github.com/milo-os/assistant/internal/gapreport"
	"github.com/milo-os/assistant/internal/history"
	"github.com/milo-os/assistant/pkg/apis/assistant/install"
	"github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// Scheme and Codecs hold the assistant group's internal + versioned types and
// their serializers, shared by the options (legacy codec) and the installed
// API group.
var (
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)
)

func init() {
	install.Install(Scheme)
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Version: "v1"})
	unversioned := schema.GroupVersion{Group: "", Version: "v1"}
	Scheme.AddUnversionedTypes(unversioned,
		&metav1.Status{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	)
}

// ExtraConfig carries the assistant-group backends into New(): the read-only
// views over the shared conversation store and the shared gap-report store.
type ExtraConfig struct {
	Reader     history.Reader
	GapReports gapreport.Store
}

// Config is the conversations apiserver config: the generic recommended config
// plus our ExtraConfig.
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

// ConversationServer is the assembled apiserver.
type ConversationServer struct {
	GenericAPIServer *genericapiserver.GenericAPIServer
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig is a Config that has been defaulted and is ready to build a
// server; the embedded pointer prevents copying an incomplete config.
type CompletedConfig struct {
	*completedConfig
}

// Complete fills in defaults and returns a config that can build a server.
func (cfg *Config) Complete() CompletedConfig {
	return CompletedConfig{&completedConfig{
		GenericConfig: cfg.GenericConfig.Complete(),
		ExtraConfig:   &cfg.ExtraConfig,
	}}
}

// New builds the generic server and installs the assistant API group with the
// bespoke Conversation storage (list/get) and the messages subresource.
func (c completedConfig) New() (*ConversationServer, error) {
	genericServer, err := c.GenericConfig.New("conversations-apiserver", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}

	s := &ConversationServer{GenericAPIServer: genericServer}

	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(v1alpha1.GroupName, Scheme, metav1.ParameterCodec, Codecs)

	v1alpha1Storage := map[string]rest.Storage{
		"conversations":          conversation.NewConversationREST(c.ExtraConfig.Reader),
		"conversations/messages": conversation.NewMessagesREST(c.ExtraConfig.Reader),
		"capabilitygapreports":   capabilitygapreport.NewCapabilityGapReportREST(c.ExtraConfig.GapReports),
	}
	apiGroupInfo.VersionedResourcesStorageMap["v1alpha1"] = v1alpha1Storage

	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	klog.Info("conversations apiserver initialized")
	return s, nil
}
