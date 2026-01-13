package routek

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/fasthttp/router"
	"github.com/go-konsultin/errk"
	"github.com/valyala/fasthttp"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultRouteFile is used when Config.RouteFile is empty.
	DefaultRouteFile = "internal/api-route.yaml"
)

type Config struct {
	// RouteFile is the path to api-route.yaml. If empty, routek searches a few sensible defaults.
	RouteFile string
	Handlers  map[string]any
	Responder *Responder
}

type (
	routeDocument map[string]serviceRoutes

	serviceRoutes struct {
		Routes []yamlRoute `yaml:"route"`
	}

	yamlRoute struct {
		Method  string
		Path    string
		Handler string
	}
)

func (r *yamlRoute) UnmarshalYAML(value *yaml.Node) error {
	// Decode into a plain map to find the HTTP method key and the handler field.
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return err
	}

	for key, val := range raw {
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "handler":
			if handler, ok := val.(string); ok {
				r.Handler = handler
			}
		case "get", "post", "put", "delete", "patch", "head", "options":
			r.Method = strings.ToUpper(lowerKey)
			path, ok := val.(string)
			if !ok {
				return fmt.Errorf("route %q must map to a path string", key)
			}
			r.Path = path
		}
	}

	if r.Method == "" {
		return errors.New("route does not declare an HTTP method")
	}

	if r.Path == "" {
		return errors.New("route does not declare a path")
	}

	if r.Handler == "" {
		return errors.New("route does not declare a handler")
	}

	return nil
}

func NewRouter(cfg Config) (*router.Router, error) {
	if len(cfg.Handlers) == 0 {
		return nil, errors.New("routek: handler registry is empty")
	}

	routeFile, err := findRouteFile(cfg.RouteFile)
	if err != nil {
		return nil, err
	}

	content, err := os.ReadFile(routeFile)
	if err != nil {
		return nil, fmt.Errorf("routek: read %s: %w", routeFile, err)
	}

	var doc routeDocument
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("routek: parse %s: %w", routeFile, err)
	}

	if len(doc) == 0 {
		return nil, fmt.Errorf("routek: no routes defined in %s", routeFile)
	}

	rt := router.New()
	rt.HandleMethodNotAllowed = false // Return 404 instead of 405 for method mismatches
	responder := cfg.Responder
	if responder == nil {
		responder = NewResponder(false)
	}

	for group, routes := range doc {
		handlerTarget, ok := cfg.Handlers[group]
		if !ok {
			return nil, fmt.Errorf("routek: handler target for group %q not provided", group)
		}

		if handlerTarget == nil {
			return nil, fmt.Errorf("routek: handler target for group %q is nil", group)
		}

		for _, r := range routes.Routes {
			handlerFn, err := buildHandler(handlerTarget, r.Handler, responder)
			if err != nil {
				return nil, fmt.Errorf("routek: %s.%s: %w", group, r.Handler, err)
			}

			rt.Handle(r.Method, r.Path, handlerFn)
		}
	}

	return rt, nil
}

func findRouteFile(path string) (string, error) {
	if path != "" {
		if exists(path) {
			return path, nil
		}

		return "", fmt.Errorf("routek: route file %q not found", path)
	}

	candidates := []string{
		DefaultRouteFile,
		"api-route.yaml",
		filepath.Join("config", "api-route.yaml"),
	}

	for _, candidate := range candidates {
		if exists(candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("routek: api-route.yaml not found (tried %v)", candidates)
}

func exists(path string) bool {
	if path == "" {
		return false
	}

	if _, err := os.Stat(path); err == nil {
		return true
	}

	return false
}

func buildHandler(target any, methodName string, responder *Responder) (fasthttp.RequestHandler, error) {
	if methodName == "" {
		return nil, errors.New("handler name is empty")
	}

	value := reflect.ValueOf(target)
	method := value.MethodByName(methodName)
	if !method.IsValid() {
		return nil, fmt.Errorf("handler %q not found on %T", methodName, target)
	}

	methodType := method.Type()
	ctxType := reflect.TypeOf(&fasthttp.RequestCtx{})
	errType := reflect.TypeOf((*error)(nil)).Elem()

	if methodType.NumIn() != 1 || methodType.In(0) != ctxType {
		return nil, fmt.Errorf("handler %q must accept exactly one *fasthttp.RequestCtx argument", methodName)
	}

	switch methodType.NumOut() {
	case 0:
		return func(ctx *fasthttp.RequestCtx) {
			method.Call([]reflect.Value{reflect.ValueOf(ctx)})
		}, nil
	case 1:
		if methodType.Out(0) != errType {
			return nil, fmt.Errorf("handler %q must return either nothing or error", methodName)
		}

		return func(ctx *fasthttp.RequestCtx) {
			if res := method.Call([]reflect.Value{reflect.ValueOf(ctx)}); len(res) == 1 && !res[0].IsNil() {
				err := res[0].Interface().(error)
				status, code, message := extractErrorInfo(err)
				responder.Error(ctx, status, code, message, err)
			}
		}, nil
	case 2:
		if methodType.Out(1) != errType {
			return nil, fmt.Errorf("handler %q must return (any, error)", methodName)
		}

		return func(ctx *fasthttp.RequestCtx) {
			res := method.Call([]reflect.Value{reflect.ValueOf(ctx)})
			data := res[0].Interface()
			if !res[1].IsNil() {
				err := res[1].Interface().(error)
				status, code, message := extractErrorInfo(err)
				responder.Error(ctx, status, code, message, err)
				return
			}

			responder.Success(ctx, fasthttp.StatusOK, CodeOK, "success", data)
		}, nil
	default:
		return nil, fmt.Errorf("handler %q must return either nothing or error", methodName)
	}
}

// extractErrorInfo extracts HTTP status, code, and message from errk.Error.
// Returns defaults if not an errk.Error.
func extractErrorInfo(err error) (int, Code, string) {
	var errkErr *errk.Error
	if errors.As(err, &errkErr) {
		status := fasthttp.StatusInternalServerError
		if s, ok := errkErr.Metadata()["http_status"].(int); ok {
			status = s
		}
		code := Code(errkErr.Code())
		message := errkErr.Message()
		return status, code, message
	}
	return fasthttp.StatusInternalServerError, CodeInternalError, "internal server error"
}
