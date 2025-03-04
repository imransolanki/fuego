package fuego

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
)

func NewOpenApiSpec() openapi3.T {
	info := &openapi3.Info{
		Title:       "OpenAPI",
		Description: openapiDescription,
		Version:     "0.0.1",
	}
	spec := openapi3.T{
		OpenAPI:  "3.1.0",
		Info:     info,
		Paths:    &openapi3.Paths{},
		Servers:  []*openapi3.Server{},
		Security: openapi3.SecurityRequirements{},
		Components: &openapi3.Components{
			Schemas:       make(map[string]*openapi3.SchemaRef),
			RequestBodies: make(map[string]*openapi3.RequestBodyRef),
			Responses:     make(map[string]*openapi3.ResponseRef),
		},
	}
	return spec
}

// Hide prevents the routes in this server or group from being included in the OpenAPI spec.
func (s *Server) Hide() *Server {
	s.DisableOpenapi = true
	return s
}

// Show allows displaying the routes. Activated by default so useless in most cases,
// but this can be useful if you deactivated the parent group.
func (s *Server) Show() *Server {
	s.DisableOpenapi = false
	return s
}

// OutputOpenAPISpec takes the OpenAPI spec and outputs it to a JSON file and/or serves it on a URL.
// Also serves a Swagger UI.
// To modify its behavior, use the [WithOpenAPIConfig] option.
func (s *Server) OutputOpenAPISpec() openapi3.T {
	// Validate
	err := s.OpenApiSpec.Validate(context.Background())
	if err != nil {
		slog.Error("Error validating spec", "error", err)
	}

	// Marshal spec to JSON
	jsonSpec, err := s.marshalSpec()
	if err != nil {
		slog.Error("Error marshalling spec to JSON", "error", err)
	}

	if !s.OpenAPIConfig.DisableSwagger {
		s.registerOpenAPIRoutes(jsonSpec)
	}

	if !s.OpenAPIConfig.DisableLocalSave {
		err := s.saveOpenAPIToFile(s.OpenAPIConfig.JsonFilePath, jsonSpec)
		if err != nil {
			slog.Error("Error saving spec to local path", "error", err, "path", s.OpenAPIConfig.JsonFilePath)
		}
	}

	return s.OpenApiSpec
}

func (s *Server) marshalSpec() ([]byte, error) {
	if s.OpenAPIConfig.PrettyFormatJson {
		return json.MarshalIndent(s.OpenApiSpec, "", "	")
	}
	return json.Marshal(s.OpenApiSpec)
}

func (s *Server) saveOpenAPIToFile(jsonSpecLocalPath string, jsonSpec []byte) error {
	jsonFolder := filepath.Dir(jsonSpecLocalPath)

	err := os.MkdirAll(jsonFolder, 0o750)
	if err != nil {
		return errors.New("error creating docs directory")
	}

	f, err := os.Create(jsonSpecLocalPath) // #nosec G304 (file path provided by developer, not by user)
	if err != nil {
		return errors.New("error creating file")
	}
	defer f.Close()

	_, err = f.Write(jsonSpec)
	if err != nil {
		return errors.New("error writing file ")
	}

	s.printOpenAPIMessage("JSON file: " + jsonSpecLocalPath)
	return nil
}

// Registers the routes to serve the OpenAPI spec and Swagger UI.
func (s *Server) registerOpenAPIRoutes(jsonSpec []byte) {
	GetStd(s, s.OpenAPIConfig.JsonUrl, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonSpec)
	})
	s.printOpenAPIMessage(fmt.Sprintf("JSON spec: %s://%s%s", s.proto(), s.Server.Addr, s.OpenAPIConfig.JsonUrl))

	if !s.OpenAPIConfig.DisableSwaggerUI {
		Register(s, Route[any, any]{
			Method: http.MethodGet,
			Path:   s.OpenAPIConfig.SwaggerUrl + "/",
		}, s.OpenAPIConfig.UIHandler(s.OpenAPIConfig.JsonUrl))
		s.printOpenAPIMessage(fmt.Sprintf("OpenAPI UI: %s://%s%s/index.html", s.proto(), s.Server.Addr, s.OpenAPIConfig.SwaggerUrl))
	}
}

func (s *Server) printOpenAPIMessage(msg string) {
	if !s.disableStartupMessages {
		slog.Info(msg)
	}
}

func validateJsonSpecUrl(jsonSpecUrl string) bool {
	jsonSpecUrlRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_]+(.json)$`)
	return jsonSpecUrlRegexp.MatchString(jsonSpecUrl)
}

func validateSwaggerUrl(swaggerUrl string) bool {
	swaggerUrlRegexp := regexp.MustCompile(`^\/[\/a-zA-Z0-9\-\_]+[a-zA-Z0-9\-\_]$`)
	return swaggerUrlRegexp.MatchString(swaggerUrl)
}

var generator = openapi3gen.NewGenerator(
	openapi3gen.UseAllExportedFields(),
)

// RegisterOpenAPIOperation registers an OpenAPI operation.
func RegisterOpenAPIOperation[T, B any](s *Server, route Route[T, B]) (*openapi3.Operation, error) {
	if route.Operation == nil {
		route.Operation = openapi3.NewOperation()
	}

	if s.tags != nil {
		route.Operation.Tags = append(route.Operation.Tags, s.tags...)
	}

	// Tags
	if !s.disableAutoGroupTags && s.groupTag != "" {
		route.Operation.Tags = append(route.Operation.Tags, s.groupTag)
	}

	for _, param := range s.params {
		route.Param(param.Type, param.Name, param.Description, param.OpenAPIParamOption)
	}

	// Request Body
	if route.Operation.RequestBody == nil {
		bodyTag := schemaTagFromType(s, *new(B))

		if bodyTag.name != "unknown-interface" {
			requestBody := newRequestBody[B](bodyTag, []string{"application/json", "application/xml"})
			s.OpenApiSpec.Components.RequestBodies[bodyTag.name] = &openapi3.RequestBodyRef{
				Value: requestBody,
			}

			// add request body to operation
			route.Operation.RequestBody = &openapi3.RequestBodyRef{
				Ref:   "#/components/requestBodies/" + bodyTag.name,
				Value: requestBody,
			}
		}
	}

	// Response - globals
	for _, openAPIGlobalResponse := range s.globalOpenAPIResponses {
		addResponse(s, route.Operation, openAPIGlobalResponse.Code, openAPIGlobalResponse.Description, openAPIGlobalResponse.ErrorType)
	}

	// Response - 200
	responseSchema := schemaTagFromType(s, *new(T))
	content := openapi3.NewContentWithSchemaRef(&responseSchema.SchemaRef, []string{"application/json", "application/xml"})
	response := openapi3.NewResponse().WithDescription("OK").WithContent(content)
	route.Operation.AddResponse(200, response)

	// Path parameters
	for _, pathParam := range parsePathParams(route.Path) {
		parameter := openapi3.NewPathParameter(pathParam)
		parameter.Schema = openapi3.NewStringSchema().NewRef()
		if strings.HasSuffix(pathParam, "...") {
			parameter.Description += " (might contain slashes)"
		}

		route.Operation.AddParameter(parameter)
	}

	s.OpenApiSpec.AddOperation(route.Path, route.Method, route.Operation)

	return route.Operation, nil
}

func newRequestBody[RequestBody any](tag schemaTag, consumes []string) *openapi3.RequestBody {
	content := openapi3.NewContentWithSchemaRef(&tag.SchemaRef, consumes)
	return openapi3.NewRequestBody().
		WithRequired(true).
		WithDescription("Request body for " + reflect.TypeOf(*new(RequestBody)).String()).
		WithContent(content)
}

// schemaTag is a struct that holds the name of the struct and the associated openapi3.SchemaRef
type schemaTag struct {
	openapi3.SchemaRef
	name string
}

func schemaTagFromType(s *Server, v any) schemaTag {
	if v == nil {
		// ensure we add unknown-interface to our schemas
		schema := s.getOrCreateSchema("unknown-interface", struct{}{})
		return schemaTag{
			name: "unknown-interface",
			SchemaRef: openapi3.SchemaRef{
				Ref:   "#/components/schemas/unknown-interface",
				Value: schema,
			},
		}
	}

	return dive(s, reflect.TypeOf(v), schemaTag{}, 5)
}

// dive returns a schemaTag which includes the generated openapi3.SchemaRef and
// the name of the struct being passed in.
// If the type is a pointer, map, channel, function, or unsafe pointer,
// it will dive into the type and return the name of the type it points to.
// If the type is a slice or array type it will dive into the type as well as
// build and openapi3.Schema where Type is array and Ref is set to the proper
// components Schema
func dive(s *Server, t reflect.Type, tag schemaTag, maxDepth int) schemaTag {
	if maxDepth == 0 {
		return schemaTag{
			name: "default",
			SchemaRef: openapi3.SchemaRef{
				Ref: "#/components/schemas/default",
			},
		}
	}

	switch t.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return dive(s, t.Elem(), tag, maxDepth-1)

	case reflect.Slice, reflect.Array:
		item := dive(s, t.Elem(), tag, maxDepth-1)
		tag.name = item.name
		tag.Value = openapi3.NewArraySchema()
		tag.Value.Items = &item.SchemaRef
		return tag

	default:
		tag.name = t.Name()
		if t.Kind() == reflect.Struct && strings.HasPrefix(tag.name, "DataOrTemplate") {
			return dive(s, t.Field(0).Type, tag, maxDepth-1)
		}
		tag.Ref = "#/components/schemas/" + tag.name
		tag.Value = s.getOrCreateSchema(tag.name, reflect.New(t).Interface())

		return tag
	}
}

// getOrCreateSchema is used to get a schema from the OpenAPI spec.
// If the schema does not exist, it will create a new schema and add it to the OpenAPI spec.
func (s *Server) getOrCreateSchema(key string, v any) *openapi3.Schema {
	schemaRef, ok := s.OpenApiSpec.Components.Schemas[key]
	if !ok {
		schemaRef = s.createSchema(key, v)
	}
	return schemaRef.Value
}

// createSchema is used to create a new schema and add it to the OpenAPI spec.
// Relies on the openapi3gen package to generate the schema, and adds custom struct tags.
func (s *Server) createSchema(key string, v any) *openapi3.SchemaRef {
	schemaRef, err := generator.NewSchemaRefForValue(v, s.OpenApiSpec.Components.Schemas)
	if err != nil {
		slog.Error("Error generating schema", "key", key, "error", err)
	}
	schemaRef.Value.Description = key + " schema"

	descriptionable, ok := v.(OpenAPIDescriptioner)
	if ok {
		schemaRef.Value.Description = descriptionable.Description()
	}

	s.parseStructTags(reflect.TypeOf(v), schemaRef)

	s.OpenApiSpec.Components.Schemas[key] = schemaRef

	return schemaRef
}

// parseStructTags parses struct tags and modifies the schema accordingly.
// t must be a struct type.
// It adds the following struct tags (tag => OpenAPI schema field):
// - description => description
// - example => example
// - json => nullable (if contains omitempty)
// - validate:
//   - required => required
//   - min=1 => min=1 (for integers)
//   - min=1 => minLength=1 (for strings)
//   - max=100 => max=100 (for integers)
//   - max=100 => maxLength=100 (for strings)
func (s *Server) parseStructTags(t reflect.Type, schemaRef *openapi3.SchemaRef) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}

	if t.Kind() != reflect.Struct {
		return
	}

	for i := range t.NumField() {
		field := t.Field(i)
		jsonFieldName := field.Tag.Get("json")
		jsonFieldName = strings.Split(jsonFieldName, ",")[0] // remove omitempty, etc
		if jsonFieldName == "-" {
			continue
		}
		if jsonFieldName == "" {
			jsonFieldName = field.Name
		}

		property := schemaRef.Value.Properties[jsonFieldName]
		if property == nil {
			slog.Warn("Property not found in schema", "property", jsonFieldName)
			continue
		}
		propertyCopy := *property
		propertyValue := *propertyCopy.Value

		// Example
		example, ok := field.Tag.Lookup("example")
		if ok {
			propertyValue.Example = example
			if propertyValue.Type.Is(openapi3.TypeInteger) {
				exNum, err := strconv.Atoi(example)
				if err != nil {
					slog.Warn("Example might be incorrect (should be integer)", "error", err)
				}
				propertyValue.Example = exNum
			}
		}

		// Validation
		validateTag, ok := field.Tag.Lookup("validate")
		validateTags := strings.Split(validateTag, ",")
		if ok && slices.Contains(validateTags, "required") {
			schemaRef.Value.Required = append(schemaRef.Value.Required, jsonFieldName)
		}
		for _, validateTag := range validateTags {
			if strings.HasPrefix(validateTag, "min=") {
				min, err := strconv.Atoi(strings.Split(validateTag, "=")[1])
				if err != nil {
					slog.Warn("Min might be incorrect (should be integer)", "error", err)
				}

				if propertyValue.Type.Is(openapi3.TypeInteger) {
					minPtr := float64(min)
					propertyValue.Min = &minPtr
				} else if propertyValue.Type.Is(openapi3.TypeString) {
					propertyValue.MinLength = uint64(min)
				}
			}
			if strings.HasPrefix(validateTag, "max=") {
				max, err := strconv.Atoi(strings.Split(validateTag, "=")[1])
				if err != nil {
					slog.Warn("Max might be incorrect (should be integer)", "error", err)
				}
				if propertyValue.Type.Is(openapi3.TypeInteger) {
					maxPtr := float64(max)
					propertyValue.Max = &maxPtr
				} else if propertyValue.Type.Is(openapi3.TypeString) {
					maxPtr := uint64(max)
					propertyValue.MaxLength = &maxPtr
				}
			}
		}

		// Description
		description, ok := field.Tag.Lookup("description")
		if ok {
			propertyValue.Description = description
		}
		jsonTag, ok := field.Tag.Lookup("json")
		if ok {
			if strings.Contains(jsonTag, ",omitempty") {
				propertyValue.Nullable = true
			}
		}
		propertyCopy.Value = &propertyValue

		schemaRef.Value.Properties[jsonFieldName] = &propertyCopy
	}
}

type OpenAPIDescriptioner interface {
	Description() string
}
