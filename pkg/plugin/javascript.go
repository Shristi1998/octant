package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/console"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dop251/goja_nodejs/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	olog "github.com/vmware-tanzu/octant/internal/log"
	"github.com/vmware-tanzu/octant/pkg/action"
	"github.com/vmware-tanzu/octant/pkg/log"
	"github.com/vmware-tanzu/octant/pkg/navigation"
	"github.com/vmware-tanzu/octant/pkg/store"
	"github.com/vmware-tanzu/octant/pkg/view/component"
)

func IsJavaScriptPlugin(pluginName string) bool {
	return path.Ext(pluginName) == ".js"
}

type pluginRuntimeFactory func(context.Context, string) (*eventloop.EventLoop, error)
type pluginClassExtractor func(*goja.Runtime) (*goja.Object, error)
type pluginMetadataExtractor func(*goja.Runtime, goja.Value) (*Metadata, error)

// JSPlugin interface represents a JavaScript plugin.
type JSPlugin interface {
	Close()
	PluginPath() string
	Metadata() *Metadata

	Navigation(ctx context.Context) (navigation.Navigation, error)
	Register(ctx context.Context, dashboardAPIAddress string) (Metadata, error)
	Print(ctx context.Context, object runtime.Object) (PrintResponse, error)
	PrintTab(ctx context.Context, object runtime.Object) (TabResponse, error)
	ObjectStatus(ctx context.Context, object runtime.Object) (ObjectStatusResponse, error)
	HandleAction(ctx context.Context, actionName string, payload action.Payload) error
	Content(ctx context.Context, contentPath string) (component.ContentResponse, error)
}

type jsPlugin struct {
	loop *eventloop.EventLoop

	metadata    *Metadata
	pluginClass *goja.Object
	pluginPath  string

	runtimeFactory    pluginRuntimeFactory
	classExtractor    pluginClassExtractor
	metadataExtractor pluginMetadataExtractor

	objectStore store.Store
	mu          sync.Mutex
	ctx         context.Context
	logger      log.Logger
}

var _ JSPlugin = (*jsPlugin)(nil)

// NewJSPlugin creates a new instances of a JavaScript plugin.
func NewJSPlugin(ctx context.Context, objectStore store.Store, pluginPath string, prf pluginRuntimeFactory, pce pluginClassExtractor, pme pluginMetadataExtractor) (*jsPlugin, error) {
	loop, err := prf(ctx, pluginPath)
	if err != nil {
		return nil, fmt.Errorf("initializing runtime: %w", err)
	}

	var pluginClass *goja.Object
	var metadata *Metadata

	errCh := make(chan error)

	loop.RunOnLoop(func(vm *goja.Runtime) {
		var err error

		buf, err := ioutil.ReadFile(pluginPath)
		if err != nil {
			errCh <- fmt.Errorf("reading script: %w", err)
		}
		program, err := goja.Compile(pluginPath, string(buf), false)
		if err != nil {
			errCh <- fmt.Errorf("compiling: %w", err)
		}
		_, err = vm.RunProgram(program)
		if err != nil {
			errCh <- fmt.Errorf("script execution: %w", err)
		}

		vm.Set("httpClient", createHTTPClientObject(vm, pluginClass))

		gc := &dashboardClient{
			objectStore: objectStore,
			vm:          vm,
			ctx:         ctx,
		}
		vm.Set("dashboardClient", createClientObject(gc))

		pluginClass, err = pce(vm)
		if err != nil {
			errCh <- fmt.Errorf("loading pluginClass: %w", err)
		}

		metadata, err = pme(vm, pluginClass)
		if err != nil {
			errCh <- fmt.Errorf("loading metadata: %w", err)
		}
		errCh <- nil

	})

	err = <-errCh
	if err != nil {
		return nil, err
	}

	plugin := &jsPlugin{
		loop:              loop,
		pluginClass:       pluginClass,
		metadata:          metadata,
		runtimeFactory:    prf,
		classExtractor:    pce,
		metadataExtractor: pme,
		pluginPath:        pluginPath,
		ctx:               ctx,
	}

	return plugin, nil
}

// Close closes the dashboard client connection.
func (t *jsPlugin) Close() {
	t.loop.Stop()
}

// PluginPath returns the pluginPath.
func (t *jsPlugin) PluginPath() string {
	return t.pluginPath
}

func (t *jsPlugin) Navigation(_ context.Context) (navigation.Navigation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	nav := navigation.Navigation{}
	errCh := make(chan error)

	t.loop.RunOnLoop(func(vm *goja.Runtime) {
		handler, err := vm.RunString("_concretePlugin.navigationHandler")
		if err != nil {
			errCh <- fmt.Errorf("unable to load navigationHandler from plugin: %w", err)
			return
		}

		cHandler, ok := goja.AssertFunction(handler)
		if !ok {
			errCh <- fmt.Errorf("navigationHandler is not callable")
			return
		}

		s, err := cHandler(t.pluginClass)
		if err != nil {
			errCh <- fmt.Errorf("calling navigationHandler: %w", err)
			return
		}

		jsonNav, err := json.Marshal(s.Export())
		if err != nil {
			errCh <- fmt.Errorf("unable to marshal navigation json: %w", err)
			return
		}

		if err := json.Unmarshal(jsonNav, &nav); err != nil {
			errCh <- fmt.Errorf("unable to unmarshal navigation json: %w", err)
			return
		}
		errCh <- nil
	})

	err := <-errCh
	if err != nil {
		return nav, err
	}

	return nav, nil
}

func (t *jsPlugin) Content(_ context.Context, contentPath string) (component.ContentResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	cr := component.ContentResponse{}
	errCh := make(chan error)

	t.loop.RunOnLoop(func(vm *goja.Runtime) {
		handler, err := vm.RunString("_concretePlugin.contentHandler")
		if err != nil {
			errCh <- fmt.Errorf("unable to load contentHandler from plugin: %w", err)
			return
		}

		cHandler, ok := goja.AssertFunction(handler)
		if !ok {
			errCh <- fmt.Errorf("contentHandler is not callable")
			return
		}
		obj := vm.NewObject()
		if err := obj.Set("contentPath", vm.ToValue(contentPath)); err != nil {
			errCh <- fmt.Errorf("unable to set contentPath: %w", err)
			return
		}
		s, err := cHandler(t.pluginClass, obj)
		if err != nil {
			errCh <- fmt.Errorf("calling contentHandler: %w", err)
			return
		}

		pluginResp := s.ToObject(vm)
		if pluginResp == nil {
			errCh <- fmt.Errorf("empty contentResponse")
			return
		}

		content := pluginResp.Get("content")
		if content == goja.Undefined() {
			errCh <- fmt.Errorf("unable to get content from contentResponse")
			return
		}

		contentObj, ok := content.Export().(map[string]interface{})
		if !ok {
			errCh <- fmt.Errorf("unable to get content as map from contentResponse")
			return
		}

		rawTitle, ok := contentObj["title"]
		if ok {
			titles, ok := rawTitle.([]interface{})
			if !ok {
				errCh <- fmt.Errorf("unable to get title array from content")
				return
			}
			for i, c := range titles {
				realTitle, err := extractComponent(fmt.Sprintf("title[%d]", i), c)
				if err != nil {
					errCh <- fmt.Errorf("unable to extract title: %w", err)
					return
				}

				title, ok := realTitle.(component.TitleComponent)
				if !ok {
					errCh <- fmt.Errorf("unable to convert component to TitleComponent")
					return
				}
				cr.Title = append(cr.Title, title)
			}
		}

		rawComponents, ok := contentObj["viewComponents"]
		if !ok {
			errCh <- fmt.Errorf("unable to get viewComponents from content")
			return
		}

		components, ok := rawComponents.([]interface{})
		if !ok {
			errCh <- fmt.Errorf("unable to get viewComponents list")
			return
		}

		for i, c := range components {
			realComponent, err := extractComponent(fmt.Sprintf("viewComponent[%d]", i), c)
			if err != nil {
				errCh <- fmt.Errorf("unable to extract component: %w", err)
				return
			}
			cr.Add(realComponent)
		}

		rawButtonGroup, ok := contentObj["buttonGroup"]
		if ok {
			realButtonGroup, err := extractComponent("buttonGroup", rawButtonGroup)
			if err != nil {
				errCh <- fmt.Errorf("unable to extract buttonGroup: %w", err)
				return
			}

			buttonGroup, ok := realButtonGroup.(*component.ButtonGroup)
			if !ok {
				errCh <- fmt.Errorf("unable to convert extracted component to buttonGroup")
				return
			}

			cr.ButtonGroup = buttonGroup
		}
		errCh <- nil
	})

	if err := <-errCh; err != nil {
		return cr, err
	}
	return cr, nil
}

func (t *jsPlugin) Metadata() *Metadata {
	return t.metadata
}

func (t *jsPlugin) Register(_ context.Context, _ string) (Metadata, error) {
	return Metadata{}, fmt.Errorf("not implemented")
}

func (t *jsPlugin) PrintTab(_ context.Context, object runtime.Object) (TabResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	tabResponse, err := t.objectRequestCall("tabHandler", object)
	if err != nil {
		return TabResponse{}, err
	}

	tab := tabResponse.Get("tab")
	if tab == goja.Undefined() {
		return TabResponse{}, fmt.Errorf("tab property not found")
	}

	contents, ok := tab.Export().(map[string]interface{})
	if !ok {
		return TabResponse{}, fmt.Errorf("unable to export tab contents")
	}

	cTab := &component.Tab{}
	if name, ok := contents["name"]; ok {
		cTab.Contents = *component.NewFlexLayout(name.(string))
	}

	if contents, ok := contents["contents"]; ok {
		realComponent, err := extractComponent("tab contents", contents)
		if err != nil {
			return TabResponse{}, fmt.Errorf("unable to extract component: %w", err)
		}
		realFlexLayout := *realComponent.(*component.FlexLayout)
		cTab.Contents.Config = realFlexLayout.Config
	}

	return TabResponse{
		Tab: cTab,
	}, nil
}

func (t *jsPlugin) ObjectStatus(_ context.Context, object runtime.Object) (ObjectStatusResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	osResponse, err := t.objectRequestCall("objectStatusHandler", object)
	if err != nil {
		return ObjectStatusResponse{}, err
	}

	objStatus := osResponse.Get("objectStatus")
	if objStatus == goja.Undefined() {
		return ObjectStatusResponse{}, fmt.Errorf("objectStatus property not found")
	}

	mapObjStatus, ok := objStatus.Export().(map[string]interface{})
	if !ok {
		return ObjectStatusResponse{}, fmt.Errorf("unable to get objectStatus map")
	}

	jsonOS, err := json.Marshal(mapObjStatus)
	if err != nil {
		return ObjectStatusResponse{}, fmt.Errorf("unable to marshal podSummary: %w", err)
	}

	var podSummary component.PodSummary
	if err := json.Unmarshal(jsonOS, &podSummary); err != nil {
		return ObjectStatusResponse{}, fmt.Errorf("unable to unmarshal podSummary: %w", err)
	}

	return ObjectStatusResponse{
		ObjectStatus: podSummary,
	}, nil
}

func (t *jsPlugin) HandleAction(_ context.Context, actionPath string, payload action.Payload) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	errCh := make(chan error)

	t.loop.RunOnLoop(func(vm *goja.Runtime) {
		handler, err := vm.RunString("_concretePlugin.actionHandler")
		if err != nil {
			errCh <- fmt.Errorf("unable to load actionHandler from plugin: %w", err)
			return
		}

		cHandler, ok := goja.AssertFunction(handler)
		if !ok {
			errCh <- fmt.Errorf("actionHandler is not callable")
			return
		}

		var pl map[string]interface{}
		pl = payload

		obj := vm.NewObject()
		if err := obj.Set("actionName", vm.ToValue(actionPath)); err != nil {
			errCh <- fmt.Errorf("unable to set actionName: %w", err)
			return
		}
		if err := obj.Set("payload", pl); err != nil {
			errCh <- fmt.Errorf("unable to set payload: %w", err)
			return
		}

		s, err := cHandler(t.pluginClass, obj)
		if err != nil {
			errCh <- fmt.Errorf("calling actionHandler: %w", err)
			return
		}

		if s != goja.Undefined() {
			if jsErr := s.ToObject(vm); jsErr != nil {
				errStr := jsErr.Get("error")
				if errStr != goja.Undefined() {
					errCh <- fmt.Errorf("%s actionHandler: %q", t.pluginPath, jsErr.Get("error"))
					return
				}
			}
		}
		errCh <- nil
	})

	if err := <-errCh; err != nil {
		return err
	}

	return nil
}

func (t *jsPlugin) Print(_ context.Context, object runtime.Object) (PrintResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	printResponse, err := t.objectRequestCall("printHandler", object)
	if err != nil {
		return PrintResponse{}, err
	}

	sections, ok := printResponse.Export().(map[string]interface{})
	if !ok {
		return PrintResponse{}, fmt.Errorf("unable to parse printHandler response sections")
	}

	var configSections []component.SummarySection
	var statusSections []component.SummarySection
	var flexItems []component.FlexLayoutItem

	for k, v := range sections {
		switch k {
		case "config":
			ss, err := extractSections(k, v)
			if err != nil {
				return PrintResponse{}, fmt.Errorf("error extracting sections: %w", err)
			}
			configSections = append(configSections, ss...)
		case "status":
			ss, err := extractSections(k, v)
			if err != nil {
				return PrintResponse{}, fmt.Errorf("error extracting sections: %w", err)
			}
			statusSections = append(statusSections, ss...)
		case "items":
			ss, err := extractItems(k, v)
			if err != nil {
				return PrintResponse{}, fmt.Errorf("error extracting items: %w", err)
			}
			flexItems = append(flexItems, ss...)
		default:
			return PrintResponse{}, fmt.Errorf("unknown printHandler response section: %s", k)
		}
	}

	var response PrintResponse
	response.Config = configSections
	response.Status = statusSections
	response.Items = flexItems

	return response, nil
}

func (t *jsPlugin) objectRequestCall(handlerName string, object runtime.Object) (*goja.Object, error) {
	errCh := make(chan error)
	var response *goja.Object

	t.loop.RunOnLoop(func(vm *goja.Runtime) {
		handler, err := vm.RunString(fmt.Sprintf("_concretePlugin.%s", handlerName))
		if err != nil {
			errCh <- fmt.Errorf("unable to load %s from plugin: %w", handlerName, err)
			return
		}

		cHandler, ok := goja.AssertFunction(handler)
		if !ok {
			errCh <- fmt.Errorf("%s is not callable", handlerName)
			return
		}

		obj := vm.NewObject()
		if err := obj.Set("object", vm.ToValue(object)); err != nil {
			errCh <- fmt.Errorf("unable to set object: %w", err)
			return
		}
		s, err := cHandler(t.pluginClass, obj)
		if err != nil {
			errCh <- err
			return
		}

		response = s.ToObject(vm)
		if response == nil {
			errCh <- fmt.Errorf("no status found")
			return
		}
		errCh <- nil
	})

	if err := <-errCh; err != nil {
		return nil, err
	}

	return response, nil
}

func extractComponent(name string, i interface{}) (component.Component, error) {
	rawComponent, ok := i.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unable to get %s map", name)
	}

	rawMetadata, ok := rawComponent["metadata"]
	if !ok {
		return nil, fmt.Errorf("unable to get metadata from %s", name)
	}

	metadataJson, err := json.Marshal(rawMetadata)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal metadata from: %s: %w", name, err)
	}

	metadata := component.Metadata{}
	if err := json.Unmarshal(metadataJson, &metadata); err != nil {
		return nil, fmt.Errorf("unable to unmarhal metadata from %s: %w", name, err)
	}

	config, ok := rawComponent["config"]
	if !ok {
		return nil, fmt.Errorf("unable to get config from %s", name)
	}

	configJson, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal buttonGroup config: %w", err)
	}

	typedObject := component.TypedObject{
		Config:   configJson,
		Metadata: metadata,
	}

	c, err := typedObject.ToComponent()
	if err != nil {
		return nil, fmt.Errorf("unable to convert buttonGroup to component: %w", err)
	}
	return c, nil
}

func extractItems(name string, i interface{}) ([]component.FlexLayoutItem, error) {
	var items []component.FlexLayoutItem

	v, ok := i.([]interface{})
	if !ok {
		return items, fmt.Errorf("unable to parse printHandler %s summary items", name)
	}

	for idx, ii := range v {
		mapItem, ok := ii.(map[string]interface{})
		if !ok {
			return items, fmt.Errorf("unable to parse %s summary items", name)
		}
		flexLayoutItem := component.FlexLayoutItem{}
		jsonSS, err := json.Marshal(mapItem)
		if err != nil {
			return items, fmt.Errorf("unable to marshal json in position %d in %s", idx, name)
		}
		if err := json.Unmarshal(jsonSS, &flexLayoutItem); err != nil {
			return items, fmt.Errorf("unable to unmarshal json in position %d in %s", idx, name)
		}
		items = append(items, flexLayoutItem)
	}

	return items, nil
}

func extractSections(name string, i interface{}) ([]component.SummarySection, error) {
	var sections []component.SummarySection

	v, ok := i.([]interface{})
	if !ok {
		return sections, fmt.Errorf("unable to parse printHandler %s summary sections", name)
	}

	for idx, ii := range v {
		mapSummarySection, ok := ii.(map[string]interface{})
		if !ok {
			return sections, fmt.Errorf("unable to parse %s summary section", name)
		}
		ss := component.SummarySection{}
		jsonSS, err := json.Marshal(mapSummarySection)
		if err != nil {
			return sections, fmt.Errorf("unable to marshal json GVK in position %d in %s", idx, name)
		}
		if err := json.Unmarshal(jsonSS, &ss); err != nil {
			return sections, fmt.Errorf("unable to unmarshal json GVK in position %d in %s", idx, name)
		}
		sections = append(sections, ss)
	}

	return sections, nil
}

func ExtractDefaultClass(vm *goja.Runtime) (*goja.Object, error) {
	// This is the location of a export default class that implements the Octant
	// TypeScript module definition.
	instantiateClass := "var _concretePlugin = new module.exports.default; _concretePlugin"
	// This is the library name the Octant webpack configuration uses.
	if vm.Get("_octantPlugin") != nil {
		instantiateClass = "var _concretePlugin = new _octantPlugin(dashboardClient, httpClient); _concretePlugin"
	}

	v, err := vm.RunString(instantiateClass)
	if err != nil {
		return nil, fmt.Errorf("unable to create new plugin: %w", err)
	}
	pluginClass := v.ToObject(vm)
	return pluginClass, nil
}

func extractActions(i interface{}) ([]string, error) {
	actions, ok := i.([]interface{})
	if !ok {
		return nil, fmt.Errorf("unable to parse ActionNames")
	}
	actionNames := make([]string, len(actions))
	for i := 0; i < len(actions); i++ {
		actionNames[i] = actions[i].(string)
	}
	return actionNames, nil
}

func extractGvk(name string, i interface{}) ([]schema.GroupVersionKind, error) {
	GVKs, ok := i.([]interface{})
	if !ok {
		return nil, fmt.Errorf("%s: unable to parse GVK list for supportPrinterConfig", name)
	}
	var gvkList []schema.GroupVersionKind
	for i, ii := range GVKs {
		mapGvk, ok := ii.(map[string]interface{})
		if !ok {
			return gvkList, fmt.Errorf("%s: unable to parse GVK in position %d in supportPrinterConfig", name, i)
		}
		gvk := schema.GroupVersionKind{}

		jsonGvk, err := json.Marshal(mapGvk)
		if err != nil {
			return gvkList, fmt.Errorf("%s: unable to marshal json GVK in position %d in supportPrinterConfig", name, i)
		}

		if err := json.Unmarshal(jsonGvk, &gvk); err != nil {
			return gvkList, fmt.Errorf("%s: unable to unmarshal json GVK in position %d in supportPrinterConfig", name, i)
		}

		gvkList = append(gvkList, gvk)
	}
	return gvkList, nil
}

func ExtractMetadata(vm *goja.Runtime, pluginValue goja.Value) (*Metadata, error) {
	metadata := new(Metadata)

	this := pluginValue.ToObject(vm)
	if this == nil {
		return nil, fmt.Errorf("unable to construct `this` from plugin class")
	}

	metadata.Name = this.Get("name").String()
	if metadata.Name == "" {
		return nil, fmt.Errorf("name is a required property")
	}

	metadata.Description = this.Get("description").String()
	if metadata.Description == "" {
		return nil, fmt.Errorf("description is a required property")
	}

	metadata.Capabilities.IsModule = this.Get("isModule").ToBoolean()

	if capability, ok := this.Get("capabilities").Export().(map[string]interface{}); ok {
		for k, v := range capability {
			switch k {
			case "supportPrinterConfig":
				GVKs, err := extractGvk(k, v)
				if err != nil {
					return nil, fmt.Errorf("extractGvks: %w", err)
				}
				metadata.Capabilities.SupportsPrinterConfig = append(metadata.Capabilities.SupportsPrinterConfig, GVKs...)
			case "supportPrinterStatus":
				GVKs, err := extractGvk(k, v)
				if err != nil {
					return nil, fmt.Errorf("extractGvks: %w", err)
				}
				metadata.Capabilities.SupportsPrinterStatus = append(metadata.Capabilities.SupportsPrinterStatus, GVKs...)
			case "supportPrinterItems":
				GVKs, err := extractGvk(k, v)
				if err != nil {
					return nil, fmt.Errorf("extractGvks: %w", err)
				}
				metadata.Capabilities.SupportsPrinterItems = append(metadata.Capabilities.SupportsPrinterItems, GVKs...)
			case "supportObjectStatus":
				GVKs, err := extractGvk(k, v)
				if err != nil {
					return nil, fmt.Errorf("extractGvks: %w", err)
				}
				metadata.Capabilities.SupportsObjectStatus = append(metadata.Capabilities.SupportsObjectStatus, GVKs...)
			case "supportTab":
				GVKs, err := extractGvk(k, v)
				if err != nil {
					return nil, fmt.Errorf("extractGvks: %w", err)
				}
				metadata.Capabilities.SupportsTab = append(metadata.Capabilities.SupportsTab, GVKs...)
			case "actionNames":
				actions, err := extractActions(v)
				if err != nil {
					return nil, fmt.Errorf("extractActions: %w", err)
				}
				metadata.Capabilities.ActionNames = append(metadata.Capabilities.ActionNames, actions...)
			default:
				fmt.Printf("unknown capabilitiy: %s\n", k)
			}
		}
	} else {
		return nil, fmt.Errorf("unable to get capabilites for plugin class")
	}

	return metadata, nil
}

func CreateRuntimeLoop(ctx context.Context, logName string) (*eventloop.EventLoop, error) {
	loop := eventloop.NewEventLoop()
	loop.Start()

	errCh := make(chan error)

	loop.RunOnLoop(func(vm *goja.Runtime) {
		vm.Set("global", vm.GlobalObject())
		vm.Set("self", vm.GlobalObject())

		_, err := vm.RunString(`
var module = { exports: {} };
var exports = module.exports;
`)
		if err != nil {
			errCh <- fmt.Errorf("runtime global values: %w", err)
			return
		}

		registry := new(require.Registry)
		registry.Enable(vm)

		logger := olog.From(ctx).With("plugin", logName)
		printer := logPrinter{logger: logger}
		registry.RegisterNativeModule("console", console.RequireWithPrinter(printer))
		console.Enable(vm)

		// This maps Caps fields to lower fields from struct to Object based on the JSON annotations.
		vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))
		errCh <- nil
	})

	err := <-errCh
	if err != nil {
		return nil, err
	}

	return loop, nil
}

type logPrinter struct {
	logger log.Logger
}

func (l logPrinter) Log(msg string) {
	l.logger.Infof(msg)
}

func (l logPrinter) Warn(msg string) {
	l.logger.Warnf(msg)
}

func (l logPrinter) Error(msg string) {
	l.logger.Errorf(msg)
}

type dashboardClient struct {
	objectStore store.Store
	vm          *goja.Runtime
	ctx         context.Context
}

func (d *dashboardClient) Delete(c goja.FunctionCall) goja.Value {
	var key store.Key
	obj := c.Argument(0).ToObject(d.vm)
	if err := d.vm.ExportTo(obj, &key); err != nil {
		return d.vm.NewTypeError(fmt.Errorf("dashboardClient.Delete: %w", err))
	}

	if err := d.objectStore.Delete(d.ctx, key); err != nil {
		return d.vm.NewGoError(err)
	}
	return goja.Undefined()
}

func (d *dashboardClient) Get(c goja.FunctionCall) goja.Value {
	var key store.Key
	obj := c.Argument(0).ToObject(d.vm)
	if err := d.vm.ExportTo(obj, &key); err != nil {
		return d.vm.NewGoError(fmt.Errorf("dashboardClient.Get: %w", err))
	}

	u, err := d.objectStore.Get(d.ctx, key)
	if err != nil {
		return d.vm.NewGoError(err)
	}

	return d.vm.ToValue(u.Object)
}

func (d *dashboardClient) List(c goja.FunctionCall) goja.Value {
	var key store.Key
	obj := c.Argument(0).ToObject(d.vm)
	if err := d.vm.ExportTo(obj, &key); err != nil {
		return d.vm.NewGoError(fmt.Errorf("dashboardClient.List: %w", err))
	}

	u, _, err := d.objectStore.List(d.ctx, key)
	if err != nil {
		return d.vm.NewGoError(err)
	}

	items := make([]interface{}, len(u.Items))
	for i := 0; i < len(u.Items); i++ {
		items[i] = u.Items[i].Object
	}

	return d.vm.ToValue(items)
}

func (d *dashboardClient) Create(c goja.FunctionCall) goja.Value {
	namespace := c.Argument(0).String()
	update := c.Argument(1).String()

	if namespace == "" {
		return d.vm.NewTypeError(fmt.Errorf("create/update: invalid namespace"))
	}

	if update == "" {
		return d.vm.NewTypeError(fmt.Errorf("create/update: empty yaml"))
	}

	results, err := d.objectStore.CreateOrUpdateFromYAML(d.ctx, namespace, update)
	if err != nil {
		return d.vm.NewTypeError(fmt.Errorf("create/update: %w", err))
	}

	return d.vm.ToValue(results)
}

func createClientObject(d *dashboardClient) goja.Value {
	obj := d.vm.NewObject()
	if err := obj.Set("Get", d.Get); err != nil {
		return d.vm.NewGoError(err)
	}
	if err := obj.Set("List", d.List); err != nil {
		return d.vm.NewGoError(err)
	}
	if err := obj.Set("Create", d.Create); err != nil {
		return d.vm.NewGoError(err)
	}
	if err := obj.Set("Update", d.Create); err != nil {
		return d.vm.NewGoError(err)
	}
	if err := obj.Set("Delete", d.Delete); err != nil {
		return d.vm.NewGoError(err)
	}
	return obj
}

type httpClient struct {
	vm   *goja.Runtime
	this *goja.Object
}

func createHTTPClientObject(vm *goja.Runtime, this *goja.Object) goja.Value {
	client := vm.NewObject()
	h := &httpClient{
		vm:   vm,
		this: this,
	}
	if err := client.Set("get", h.get); err != nil {
		return vm.NewTypeError(fmt.Errorf("httpClient.Set.get: %w", err))
	}
	if err := client.Set("getJSON", h.getJSON); err != nil {
		return vm.NewTypeError(fmt.Errorf("httpClient.Set.getJSON: %w", err))
	}
	if err := client.Set("post", h.post); err != nil {
		return vm.NewTypeError(fmt.Errorf("httpClient.Set.post: %w", err))
	}
	return client
}

func (h *httpClient) _get(c goja.FunctionCall) (goja.Callable, []byte, error) {
	if len(c.Arguments) != 2 {
		return nil, nil, fmt.Errorf("invalid arguments")
	}

	urlArg := c.Argument(0).String()
	if urlArg == "" {
		return nil, nil, fmt.Errorf("empty url")
	}

	callbackArg := c.Argument(1).ToObject(h.vm)
	callback, ok := goja.AssertFunction(callbackArg)
	if !ok || callback == nil {
		return nil, nil, fmt.Errorf("bad callback function")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	r, err := client.Get(urlArg)
	if err != nil {
		return nil, nil, fmt.Errorf("get: %w", err)
	}
	defer func() {
		_ = r.Body.Close()
	}()
	response, err := ioutil.ReadAll(r.Body)
	return callback, response, err
}

func (h *httpClient) get(c goja.FunctionCall) goja.Value {
	callback, response, err := h._get(c)
	if err != nil {
		return h.vm.NewTypeError(fmt.Errorf("get: %w", err))
	}
	cr, err := callback(h.this, h.vm.ToValue(response))
	if err != nil {
		return h.vm.NewTypeError(fmt.Errorf("get: %w", err))
	}
	return cr
}

func (h *httpClient) getJSON(c goja.FunctionCall) goja.Value {
	callback, response, err := h._get(c)
	if err != nil {
		return h.vm.NewTypeError(fmt.Errorf("getJSON: %w", err))
	}

	var target interface{}
	if err := json.NewDecoder(bytes.NewReader(response)).Decode(&target); err != nil {
		return h.vm.NewTypeError(fmt.Errorf("decoding: %w", err))
	}

	cr, err := callback(h.this, h.vm.ToValue(target))
	if err != nil {
		return h.vm.NewTypeError(fmt.Errorf("getJSON: %w", err))
	}
	return cr
}

func (h *httpClient) post(_ goja.FunctionCall) goja.Value {
	return h.vm.NewGoError(fmt.Errorf("not implemented"))
}
