package render

import (
	"context"
	"fmt"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// MaxLookupStubs caps how many stubs one request may carry.
const MaxLookupStubs = 256

// stubListName is the name a wildcard stub carries when it is listed rather
// than fetched by name: a list has no requested name to give it.
const stubListName = "preview-stub"

// LookupStub is an object the template `lookup` function returns instead of
// the empty result a client-only render gives. It exists for PREVIEW: a chart
// that gates a resource on `lookup` of another one (a Krateo gate) renders
// nothing behind the gate without a cluster, so a preview can supply the
// objects the gate waits for and see what renders once it opens.
//
// A stub matches a lookup by apiVersion and kind. Name and Namespace narrow
// it when set; left empty, the stub answers every name (or namespace) — a
// gate's name is a Helm expression the caller cannot always resolve. An exact
// name match wins over a wildcard. The returned object is a copy of Object
// with apiVersion, kind and metadata.name/namespace set to what was looked up.
//
// Nothing is ever applied: the render stays client-only, stubs live for one
// request, and a request without stubs renders exactly as before.
type LookupStub struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Namespace  string                 `json:"namespace,omitempty"`
	Name       string                 `json:"name,omitempty"`
	Object     map[string]interface{} `json:"object,omitempty"`
}

// LookupCall is one distinct `lookup` a stubbed render made, and whether a
// stub answered it. Name is empty for a list lookup.
type LookupCall struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Stubbed    bool   `json:"stubbed"`
}

// ValidateLookupStubs checks a request's stubs. Its errors are client errors.
func ValidateLookupStubs(stubs []LookupStub) error {
	if len(stubs) > MaxLookupStubs {
		return fmt.Errorf("lookupStubs: %d stubs, over the limit of %d", len(stubs), MaxLookupStubs)
	}
	for i, s := range stubs {
		if strings.TrimSpace(s.APIVersion) == "" || strings.TrimSpace(s.Kind) == "" {
			return fmt.Errorf("lookupStubs[%d]: apiVersion and kind are required", i)
		}
	}
	return nil
}

// stubProvider is the engine.ClientProvider a stubbed render looks objects up
// through. It records every distinct lookup, in the order first made.
type stubProvider struct {
	stubs []LookupStub

	mu    sync.Mutex
	seen  map[LookupCall]bool
	calls []LookupCall
}

func newStubProvider(stubs []LookupStub) *stubProvider {
	return &stubProvider{stubs: stubs, seen: map[LookupCall]bool{}}
}

func (p *stubProvider) GetClientFor(apiVersion, kind string) (dynamic.NamespaceableResourceInterface, bool, error) {
	// Namespaced either way: helm scopes the client only when a namespace is
	// given, and an unscoped client matches stubs in every namespace — which
	// is what a cluster-scoped lookup (namespace "") needs.
	return &stubResource{p: p, apiVersion: apiVersion, kind: kind}, true, nil
}

func (p *stubProvider) record(call LookupCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen[call] {
		p.seen[call] = true
		p.calls = append(p.calls, call)
	}
}

func (p *stubProvider) lookups() []LookupCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]LookupCall{}, p.calls...)
}

// match finds the stub answering a lookup by name: an exact name first, then
// a wildcard.
func (p *stubProvider) match(apiVersion, kind, namespace, name string) (LookupStub, bool) {
	var wildcard *LookupStub
	for i := range p.stubs {
		s := &p.stubs[i]
		if s.APIVersion != apiVersion || s.Kind != kind || !namespaceMatches(s.Namespace, namespace) {
			continue
		}
		if s.Name == name {
			return *s, true
		}
		if s.Name == "" && wildcard == nil {
			wildcard = s
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return LookupStub{}, false
}

func namespaceMatches(stubNamespace, lookedUp string) bool {
	return stubNamespace == "" || lookedUp == "" || stubNamespace == lookedUp
}

// stubResource answers helm's lookup. The embedded interface is nil: helm's
// lookup only ever calls Namespace, Get and List, so anything else panicking
// is a helm change this code has to follow, and the render recovers it into an
// error rather than crashing.
type stubResource struct {
	dynamic.NamespaceableResourceInterface

	p          *stubProvider
	apiVersion string
	kind       string
	namespace  string
}

func (r *stubResource) Namespace(ns string) dynamic.ResourceInterface {
	scoped := *r
	scoped.namespace = ns
	return &scoped
}

func (r *stubResource) Get(_ context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	stub, ok := r.p.match(r.apiVersion, r.kind, r.namespace, name)
	r.p.record(LookupCall{APIVersion: r.apiVersion, Kind: r.kind, Namespace: r.namespace, Name: name, Stubbed: ok})
	if !ok {
		// helm turns NotFound into the empty map — the client-only answer.
		return nil, apierrors.NewNotFound(r.groupResource(), name)
	}
	return r.object(stub, name, r.namespace), nil
}

func (r *stubResource) List(_ context.Context, _ metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(r.apiVersion)
	list.SetKind(r.kind + "List")
	for _, s := range r.p.stubs {
		if s.APIVersion != r.apiVersion || s.Kind != r.kind || !namespaceMatches(s.Namespace, r.namespace) {
			continue
		}
		name := s.Name
		if name == "" {
			name = stubListName
		}
		namespace := s.Namespace
		if namespace == "" {
			namespace = r.namespace
		}
		list.Items = append(list.Items, *r.object(s, name, namespace))
	}
	r.p.record(LookupCall{APIVersion: r.apiVersion, Kind: r.kind, Namespace: r.namespace, Stubbed: len(list.Items) > 0})
	if len(list.Items) == 0 {
		return nil, apierrors.NewNotFound(r.groupResource(), "")
	}
	return list, nil
}

func (r *stubResource) object(s LookupStub, name, namespace string) *unstructured.Unstructured {
	content := map[string]interface{}{}
	if s.Object != nil {
		content = runtime.DeepCopyJSON(s.Object)
	}
	obj := &unstructured.Unstructured{Object: content}
	obj.SetAPIVersion(r.apiVersion)
	obj.SetKind(r.kind)
	obj.SetName(name)
	if namespace != "" {
		obj.SetNamespace(namespace)
	}
	return obj
}

func (r *stubResource) groupResource() schema.GroupResource {
	gv, _ := schema.ParseGroupVersion(r.apiVersion)
	return schema.GroupResource{Group: gv.Group, Resource: strings.ToLower(r.kind)}
}
