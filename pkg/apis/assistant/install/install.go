// Package install registers the assistant API group with a runtime scheme.
package install

import (
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"

	"github.com/milo-os/assistant/pkg/apis/assistant"
	"github.com/milo-os/assistant/pkg/apis/assistant/v1alpha1"
)

// Install registers both the internal types and the v1alpha1 versioned types
// (and conversion functions) for the assistant API group.
func Install(scheme *runtime.Scheme) {
	utilruntime.Must(assistant.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(scheme.SetVersionPriority(v1alpha1.SchemeGroupVersion))
}
