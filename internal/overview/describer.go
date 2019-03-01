package overview

import (
	"context"
	"reflect"

	"github.com/heptio/developer-dash/internal/log"
	"github.com/heptio/developer-dash/internal/overview/logviewer"
	"github.com/heptio/developer-dash/internal/overview/yamlviewer"
	"github.com/heptio/developer-dash/internal/portforward"

	"github.com/heptio/developer-dash/internal/queryer"

	"github.com/heptio/developer-dash/internal/overview/resourceviewer"

	"github.com/heptio/developer-dash/internal/cache"
	"github.com/heptio/developer-dash/internal/overview/printer"
	"github.com/heptio/developer-dash/internal/view/component"

	"github.com/heptio/developer-dash/internal/cluster"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/scheme"
)

type LoaderFunc func(ctx context.Context, c cache.Cache, namespace string, fields map[string]string) (*unstructured.Unstructured, error)

// Returns a loader that loads a single object from the cache
var DefaultLoader = func(cacheKey cache.Key) LoaderFunc {
	return func(ctx context.Context, c cache.Cache, namespace string, fields map[string]string) (*unstructured.Unstructured, error) {
		return loadObject(ctx, c, namespace, fields, cacheKey)
	}
}

// DescriberOptions provides options to describers
type DescriberOptions struct {
	Queryer        queryer.Queryer
	Cache          cache.Cache
	Fields         map[string]string
	Printer        printer.Printer
	Selector       labels.Selector
	PortForwardSvc portforward.PortForwardInterface
}

// Describer creates content.
type Describer interface {
	Describe(ctx context.Context, prefix, namespace string, clusterClient cluster.ClientInterface, options DescriberOptions) (component.ContentResponse, error)
	PathFilters(namespace string) []pathFilter
}

type baseDescriber struct{}

func newBaseDescriber() *baseDescriber {
	return &baseDescriber{}
}

type ListDescriber struct {
	*baseDescriber

	path       string
	title      string
	listType   func() interface{}
	objectType func() interface{}
	cacheKey   cache.Key
}

func NewListDescriber(p, title string, cacheKey cache.Key, listType, objectType func() interface{}) *ListDescriber {
	return &ListDescriber{
		path:          p,
		title:         title,
		baseDescriber: newBaseDescriber(),
		cacheKey:      cacheKey,
		listType:      listType,
		objectType:    objectType,
	}
}

// Describe creates content.
func (d *ListDescriber) Describe(ctx context.Context, prefix, namespace string, clusterClient cluster.ClientInterface, options DescriberOptions) (component.ContentResponse, error) {
	if options.Printer == nil {
		return emptyContentResponse, errors.New("object list describer requires a printer")
	}

	// Pass through selector if provided to filter objects
	var key = d.cacheKey // copy
	key.Selector = options.Selector

	objects, err := loadObjects(ctx, options.Cache, namespace, options.Fields, []cache.Key{key})
	if err != nil {
		return emptyContentResponse, err
	}

	list := component.NewList(d.title, nil)

	listType := d.listType()

	v := reflect.ValueOf(listType)
	f := reflect.Indirect(v).FieldByName("Items")

	// Convert unstructured objects to typed runtime objects
	for _, object := range objects {
		item := d.objectType()
		if err := scheme.Scheme.Convert(object, item, nil); err != nil {
			return emptyContentResponse, err
		}

		if err := copyObjectMeta(item, object); err != nil {
			return emptyContentResponse, err
		}

		newSlice := reflect.Append(f, reflect.ValueOf(item).Elem())
		f.Set(newSlice)
	}

	listObject, ok := listType.(runtime.Object)
	if !ok {
		return emptyContentResponse, errors.Errorf("expected list to be a runtime object. It was a %T",
			listType)
	}

	viewComponent, err := options.Printer.Print(listObject)
	if err != nil {
		return emptyContentResponse, err
	}

	if viewComponent != nil {
		list.Add(viewComponent)
	}

	return component.ContentResponse{
		ViewComponents: []component.ViewComponent{list},
	}, nil
}

func (d *ListDescriber) PathFilters(namespace string) []pathFilter {
	return []pathFilter{
		*newPathFilter(d.path, d),
	}
}

type ObjectDescriber struct {
	*baseDescriber

	path                  string
	baseTitle             string
	objectType            func() interface{}
	loaderFunc            LoaderFunc
	disableResourceViewer bool
}

func NewObjectDescriber(p, baseTitle string, loaderFunc LoaderFunc, objectType func() interface{}, disableResourceViewer bool) *ObjectDescriber {
	return &ObjectDescriber{
		path:                  p,
		baseTitle:             baseTitle,
		baseDescriber:         newBaseDescriber(),
		loaderFunc:            loaderFunc,
		objectType:            objectType,
		disableResourceViewer: disableResourceViewer,
	}
}

func (d *ObjectDescriber) Describe(ctx context.Context, prefix, namespace string, clusterClient cluster.ClientInterface, options DescriberOptions) (component.ContentResponse, error) {
	logger := log.From(ctx)

	if options.Printer == nil {
		return emptyContentResponse, errors.New("object describer requires a printer")
	}

	object, err := d.loaderFunc(ctx, options.Cache, namespace, options.Fields)
	if err != nil {
		return emptyContentResponse, err
	}

	if object == nil {
		return emptyContentResponse, errors.Errorf("object not found")
	}

	item := d.objectType()

	if err := scheme.Scheme.Convert(object, item, nil); err != nil {
		return emptyContentResponse, err
	}

	if err := copyObjectMeta(item, object); err != nil {
		return emptyContentResponse, errors.Wrapf(err, "copying object metadata")
	}

	objectName := object.GetName()

	var title []component.TitleViewComponent

	if objectName == "" {
		title = append(title, component.NewText(d.baseTitle))
	} else {
		title = append(title, component.NewText(d.baseTitle),
			component.NewText(objectName))
	}

	newObject, ok := item.(runtime.Object)
	if !ok {
		return emptyContentResponse, errors.Errorf("expected item to be a runtime object. It was a %T",
			item)
	}

	vc, err := options.Printer.Print(newObject)
	if err != nil {
		return emptyContentResponse, err
	}

	vc.SetAccessor("summary")
	cr := component.NewContentResponse(title)
	cr.Add(vc)

	if !d.disableResourceViewer {
		rv, err := resourceviewer.New(logger, options.Cache, resourceviewer.WithDefaultQueryer(options.Queryer))
		if err != nil {
			return emptyContentResponse, err
		}

		resourceViewerComponent, err := rv.Visit(newObject)
		if err != nil {
			return emptyContentResponse, err
		}

		resourceViewerComponent.SetAccessor("resourceViewer")
		cr.Add(resourceViewerComponent)

	}

	yvComponent, err := yamlviewer.ToComponent(newObject)
	if err != nil {
		return emptyContentResponse, err
	}

	yvComponent.SetAccessor("yaml")
	cr.Add(yvComponent)

	if isPod(newObject) {
		logsComponent, err := logviewer.ToComponent(newObject)
		if err != nil {
			return emptyContentResponse, err
		}

		logsComponent.SetAccessor("logs")
		cr.Add(logsComponent)
	}

	return *cr, nil
}

func (d *ObjectDescriber) PathFilters(namespace string) []pathFilter {
	return []pathFilter{
		*newPathFilter(d.path, d),
	}
}

func copyObjectMeta(to interface{}, from *unstructured.Unstructured) error {
	object, ok := to.(metav1.Object)
	if !ok {
		return errors.Errorf("%T is not an object", to)
	}

	t, err := meta.TypeAccessor(object)
	if err != nil {
		return errors.Wrapf(err, "accessing type meta")
	}
	t.SetAPIVersion(from.GetAPIVersion())
	t.SetKind(from.GetObjectKind().GroupVersionKind().Kind)

	object.SetNamespace(from.GetNamespace())
	object.SetName(from.GetName())
	object.SetGenerateName(from.GetGenerateName())
	object.SetUID(from.GetUID())
	object.SetResourceVersion(from.GetResourceVersion())
	object.SetGeneration(from.GetGeneration())
	object.SetSelfLink(from.GetSelfLink())
	object.SetCreationTimestamp(from.GetCreationTimestamp())
	object.SetDeletionTimestamp(from.GetDeletionTimestamp())
	object.SetDeletionGracePeriodSeconds(from.GetDeletionGracePeriodSeconds())
	object.SetLabels(from.GetLabels())
	object.SetAnnotations(from.GetAnnotations())
	object.SetInitializers(from.GetInitializers())
	object.SetOwnerReferences(from.GetOwnerReferences())
	object.SetClusterName(from.GetClusterName())
	object.SetFinalizers(from.GetFinalizers())

	return nil
}

// SectionDescriber is a wrapper to combine content from multiple describers.
type SectionDescriber struct {
	path       string
	title      string
	describers []Describer
}

// NewSectionDescriber creates a SectionDescriber.
func NewSectionDescriber(p, title string, describers ...Describer) *SectionDescriber {
	return &SectionDescriber{
		path:       p,
		title:      title,
		describers: describers,
	}
}

// Describe generates content.
func (d *SectionDescriber) Describe(ctx context.Context, prefix, namespace string, clusterClient cluster.ClientInterface, options DescriberOptions) (component.ContentResponse, error) {
	list := component.NewList(d.title, nil)

	for _, child := range d.describers {
		cResponse, err := child.Describe(ctx, prefix, namespace, clusterClient, options)
		if err != nil {
			return emptyContentResponse, err
		}

		for _, vc := range cResponse.ViewComponents {
			if nestedList, ok := vc.(*component.List); ok {
				list.Add(nestedList.Config.Items...)
			}
		}
	}

	cr := component.ContentResponse{
		ViewComponents: []component.ViewComponent{list},
		Title:          component.Title(component.NewText(d.title)),
	}

	return cr, nil
}

func (d *SectionDescriber) PathFilters(namespace string) []pathFilter {
	pathFilters := []pathFilter{
		*newPathFilter(d.path, d),
	}

	for _, child := range d.describers {
		pathFilters = append(pathFilters, child.PathFilters(namespace)...)
	}

	return pathFilters
}

func isPod(object runtime.Object) bool {
	gvk := object.GetObjectKind().GroupVersionKind()
	apiVersion, kind := gvk.ToAPIVersionAndKind()
	return apiVersion == "v1" && kind == "Pod"
}
