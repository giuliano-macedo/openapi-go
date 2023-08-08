package openapi31

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/swaggest/jsonschema-go"
	"github.com/swaggest/openapi-go"
	"github.com/swaggest/openapi-go/internal"
	"github.com/swaggest/refl"
)

// Reflector builds OpenAPI Schema with reflected structures.
type Reflector struct {
	jsonschema.Reflector
	Spec *Spec
}

// NewReflector creates an instance of OpenAPI 3.1 reflector.
func NewReflector() *Reflector {
	r := &Reflector{}
	r.SpecEns()

	return r
}

// NewOperationContext initializes openapi.OperationContext to be prepared
// and added later with Reflector.AddOperation.
func (r *Reflector) NewOperationContext(method, pathPattern string) (openapi.OperationContext, error) {
	method, pathPattern, pathParams, err := openapi.SanitizeMethodPath(method, pathPattern)
	if err != nil {
		return nil, err
	}

	pathItem := r.SpecEns().PathsEns().MapOfPathItemValues[pathPattern]

	operation, err := pathItem.Operation(method)
	if err != nil {
		return nil, err
	}

	if operation != nil {
		return nil, fmt.Errorf("operation already exists: %s %s", method, pathPattern)
	}

	operation = &Operation{}

	pathParamsMap := make(map[string]bool, len(pathParams))
	for _, p := range pathParams {
		pathParamsMap[p] = true
	}

	oc := operationContext{
		OperationContext: internal.NewOperationContext(method, pathPattern),
		op:               operation,
		pathParams:       pathParamsMap,
	}

	return oc, nil
}

// ResolveJSONSchemaRef builds JSON Schema from OpenAPI Component Schema reference.
//
// Can be used in jsonschema.Schema IsTrivial().
func (r *Reflector) ResolveJSONSchemaRef(ref string) (s jsonschema.SchemaOrBool, found bool) {
	if r.Spec == nil || r.Spec.Components == nil || r.Spec.Components.Schemas == nil ||
		!strings.HasPrefix(ref, componentsSchemas) {
		return s, false
	}

	ref = strings.TrimPrefix(ref, componentsSchemas)
	os, found := r.Spec.Components.Schemas[ref]

	if found {
		if err := s.FromSimpleMap(os); err != nil {
			panic(err)
		}
	}

	return s, found
}

// joinErrors joins non-nil errors.
func joinErrors(errs ...error) error {
	join := ""

	for _, err := range errs {
		if err != nil {
			join += ", " + err.Error()
		}
	}

	if join != "" {
		return errors.New(join[2:])
	}

	return nil
}

// SpecEns ensures returned Spec is not nil.
func (r *Reflector) SpecEns() *Spec {
	if r.Spec == nil {
		r.Spec = &Spec{Openapi: "3.1.0"}
	}

	return r.Spec
}

type operationContext struct {
	*internal.OperationContext

	op *Operation

	pathParams map[string]bool
}

// OperationExposer grants access to underlying *Operation.
type OperationExposer interface {
	Operation() *Operation
}

func (o operationContext) AddSecurity(securityName string, scopes ...string) {
	if scopes == nil {
		scopes = []string{}
	}

	o.op.Security = append(o.op.Security, map[string][]string{securityName: scopes})
}

func (o operationContext) SetTags(tags ...string) {
	o.op.WithTags(tags...)
}

func (o operationContext) SetIsDeprecated(isDeprecated bool) {
	o.op.WithDeprecated(isDeprecated)
}

func (o operationContext) SetSummary(summary string) {
	o.op.WithSummary(summary)
}

func (o operationContext) SetDescription(description string) {
	o.op.WithDescription(description)
}

func (o operationContext) SetID(operationID string) {
	o.op.WithID(operationID)
}

func (o operationContext) UnknownParamsAreForbidden(in openapi.In) bool {
	return o.op.UnknownParamIsForbidden(ParameterIn(in))
}

// Operation returns OpenAPI 3 operation for customization.
func (o operationContext) Operation() *Operation {
	return o.op
}

// AddOperation configures operation request and response schema.
func (r *Reflector) AddOperation(oc openapi.OperationContext) error {
	c, ok := oc.(operationContext)
	if !ok {
		return fmt.Errorf("wrong operation context %T received, %T expected", oc, operationContext{})
	}

	if err := r.setupRequest(c.op, oc); err != nil {
		return fmt.Errorf("setup request %s %s: %w", oc.Method(), oc.PathPattern(), err)
	}

	if err := c.op.validatePathParams(c.pathParams); err != nil {
		return fmt.Errorf("validate path params %s %s: %w", oc.Method(), oc.PathPattern(), err)
	}

	if err := r.setupResponse(c.op, oc); err != nil {
		return fmt.Errorf("setup response %s %s: %w", oc.Method(), oc.PathPattern(), err)
	}

	return r.SpecEns().AddOperation(oc.Method(), oc.PathPattern(), *c.op)
}

func (r *Reflector) setupRequest(o *Operation, oc openapi.OperationContext) error {
	for _, cu := range oc.Request() {
		switch cu.ContentType {
		case "":
			if err := joinErrors(
				r.parseParameters(o, oc, cu),
				r.parseRequestBody(o, oc, cu, mimeJSON, oc.Method(), nil, tagJSON),
				r.parseRequestBody(o, oc, cu, mimeFormUrlencoded, oc.Method(), cu.FieldMapping(openapi.InFormData), tagFormData, tagForm),
			); err != nil {
				return err
			}
		case mimeJSON:
			if err := joinErrors(
				r.parseParameters(o, oc, cu),
				r.parseRequestBody(o, oc, cu, mimeJSON, oc.Method(), nil, tagJSON),
			); err != nil {
				return err
			}
		case mimeFormUrlencoded, mimeMultipart:
			if err := joinErrors(
				r.parseParameters(o, oc, cu),
				r.parseRequestBody(o, oc, cu, mimeFormUrlencoded, oc.Method(), cu.FieldMapping(openapi.InFormData), tagFormData, tagForm),
			); err != nil {
				return err
			}
		default:
			r.stringRequestBody(o, cu.ContentType, cu.Format)
		}

		if cu.Description != "" && o.RequestBody != nil && o.RequestBody.RequestBody != nil {
			o.RequestBody.RequestBody.WithDescription(cu.Description)
		}
	}

	return nil
}

const (
	tagJSON            = "json"
	tagFormData        = "formData"
	tagForm            = "form"
	tagHeader          = "header"
	mimeJSON           = "application/json"
	mimeFormUrlencoded = "application/x-www-form-urlencoded"
	mimeMultipart      = "multipart/form-data"

	componentsSchemas = "#/components/schemas/"
)

func mediaType(format string) MediaType {
	schema := jsonschema.String.ToSchemaOrBool()
	if format != "" {
		schema.TypeObject.WithFormat(format)
	}

	sm, err := schema.ToSimpleMap()
	if err != nil {
		panic("BUG: " + err.Error())
	}

	mt := MediaType{
		Schema: sm,
	}

	return mt
}

func (r *Reflector) stringRequestBody(
	o *Operation,
	mime string,
	format string,
) {
	o.RequestBodyEns().RequestBodyEns().WithContentItem(mime, mediaType(format))
}

func (r *Reflector) parseRequestBody(
	o *Operation,
	oc openapi.OperationContext,
	cu openapi.ContentUnit,
	mime string,
	httpMethod string,
	mapping map[string]string,
	tag string,
	additionalTags ...string,
) error {
	input := cu.Structure

	httpMethod = strings.ToUpper(httpMethod)
	_, forceRequestBody := input.(openapi.RequestBodyEnforcer)
	_, forceJSONRequestBody := input.(openapi.RequestJSONBodyEnforcer)

	// GET, HEAD, DELETE and TRACE requests should not have body.
	switch httpMethod {
	case http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodTrace:
		if !forceRequestBody {
			return nil
		}
	}

	hasTaggedFields := refl.HasTaggedFields(input, tag)
	for _, t := range additionalTags {
		if hasTaggedFields {
			break
		}

		hasTaggedFields = refl.HasTaggedFields(input, t)
	}

	// Form data can not have map or array as body.
	if !hasTaggedFields && len(mapping) == 0 && tag != tagJSON {
		return nil
	}

	// If `formData` is defined on a request body `json` is ignored.
	if tag == tagJSON && refl.HasTaggedFields(input, tagFormData) && !forceJSONRequestBody {
		return nil
	}

	// JSON can be a map or array without field tags.
	if !hasTaggedFields && len(mapping) == 0 && !refl.IsSliceOrMap(input) && refl.FindEmbeddedSliceOrMap(input) == nil {
		return nil
	}

	hasFileUpload := false
	definitionPrefix := ""

	if tag != tagJSON {
		definitionPrefix += strings.Title(tag)
	}

	schema, err := r.Reflect(input,
		openapi.WithOperationCtx(oc, false, "body"),
		jsonschema.DefinitionsPrefix(componentsSchemas+definitionPrefix),
		jsonschema.RootRef,
		jsonschema.PropertyNameMapping(mapping),
		jsonschema.PropertyNameTag(tag, additionalTags...),
		sanitizeDefName,
		jsonschema.InterceptSchema(func(params jsonschema.InterceptSchemaParams) (stop bool, err error) {
			vv := params.Value.Interface()

			found := false
			if _, ok := vv.(*multipart.File); ok {
				found = true
			}

			if _, ok := vv.(*multipart.FileHeader); ok {
				found = true
			}

			if found {
				params.Schema.AddType(jsonschema.String)
				params.Schema.WithFormat("binary")

				hasFileUpload = true

				return true, nil
			}

			return false, nil
		}),
	)
	if err != nil {
		return err
	}

	definitions := schema.Definitions
	schema.Definitions = nil

	sm, err := schema.ToSchemaOrBool().ToSimpleMap()
	if err != nil {
		return err
	}

	mt := MediaType{
		Schema: sm,
	}

	for name, def := range definitions {
		sm, err := def.ToSimpleMap()
		if err != nil {
			return err
		}

		r.SpecEns().ComponentsEns().WithSchemasItem(definitionPrefix+name, sm)
	}

	if mime == mimeFormUrlencoded && hasFileUpload {
		mime = mimeMultipart
	}

	o.RequestBodyEns().RequestBodyEns().WithContentItem(mime, mt)

	return nil
}

const (
	// xForbidUnknown is a prefix of a vendor extension to indicate forbidden unknown parameters.
	// It should be used together with ParameterIn as a suffix.
	xForbidUnknown = "x-forbid-unknown-"
)

func (r *Reflector) parseParameters(o *Operation, oc openapi.OperationContext, cu openapi.ContentUnit) error {
	return joinErrors(r.parseParametersIn(o, oc, cu, openapi.InQuery, tagForm),
		r.parseParametersIn(o, oc, cu, openapi.InPath),
		r.parseParametersIn(o, oc, cu, openapi.InCookie),
		r.parseParametersIn(o, oc, cu, openapi.InHeader),
	)
}

func (r *Reflector) parseParametersIn(
	o *Operation,
	oc openapi.OperationContext,
	c openapi.ContentUnit,
	in openapi.In,
	additionalTags ...string,
) error {
	input := c.Structure
	propertyMapping := c.FieldMapping(in)

	if refl.IsSliceOrMap(input) {
		return nil
	}

	defNamePrefix := strings.Title(string(in))
	definitionsPrefix := componentsSchemas + defNamePrefix

	s, err := r.Reflect(input,
		openapi.WithOperationCtx(oc, false, in),
		jsonschema.DefinitionsPrefix(definitionsPrefix),
		jsonschema.CollectDefinitions(r.collectDefinition(defNamePrefix)),
		jsonschema.PropertyNameMapping(propertyMapping),
		jsonschema.PropertyNameTag(string(in), additionalTags...),
		func(rc *jsonschema.ReflectContext) {
			rc.UnnamedFieldWithTag = true
		},
		sanitizeDefName,
		jsonschema.SkipEmbeddedMapsSlices,
		jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
			if !params.Processed || len(params.Path) > 1 {
				return nil
			}

			name := params.Name
			propertySchema := params.PropertySchema
			field := params.Field

			sm, err := propertySchema.ToSchemaOrBool().ToSimpleMap()
			if err != nil {
				return err
			}

			p := Parameter{
				Name:        name,
				In:          ParameterIn(in),
				Description: propertySchema.Description,
				Schema:      sm,
				Content:     nil,
			}

			swg2CollectionFormat := ""
			refl.ReadStringTag(field.Tag, "collectionFormat", &swg2CollectionFormat)
			switch swg2CollectionFormat {
			case "csv":
				p.WithStyle(ParameterStyleForm).WithExplode(false)
			case "ssv":
				p.WithStyle(ParameterStyleSpaceDelimited).WithExplode(false)
			case "pipes":
				p.WithStyle(ParameterStylePipeDelimited).WithExplode(false)
			case "multi":
				p.WithStyle(ParameterStyleForm).WithExplode(true)
			}

			// Check if parameter is an JSON encoded object.
			property := reflect.New(field.Type).Interface()
			if refl.HasTaggedFields(property, tagJSON) && !refl.HasTaggedFields(property, string(in)) { //nolint:nestif
				propertySchema, err := r.Reflect(property,
					openapi.WithOperationCtx(oc, false, in),
					jsonschema.DefinitionsPrefix(definitionsPrefix),
					jsonschema.CollectDefinitions(r.collectDefinition(defNamePrefix)),
					jsonschema.RootRef,
					sanitizeDefName,
				)
				if err != nil {
					return err
				}

				sm, err := propertySchema.ToSchemaOrBool().ToSimpleMap()
				if err != nil {
					return err
				}

				p.Schema = nil
				p.WithContentItem("application/json", MediaType{Schema: sm})
			} else {
				ps, err := r.Reflect(reflect.New(field.Type).Interface(),
					openapi.WithOperationCtx(oc, false, in),
					jsonschema.InlineRefs,
					sanitizeDefName,
				)
				if err != nil {
					return err
				}

				if ps.HasType(jsonschema.Object) {
					p.WithStyle(ParameterStyleDeepObject).WithExplode(true)
				}
			}

			err = refl.PopulateFieldsFromTags(&p, field.Tag)
			if err != nil {
				return err
			}

			if in == openapi.InPath {
				p.WithRequired(true)
			}

			alreadyExists := false
			for _, ep := range o.Parameters {
				if ep.Parameter != nil && ep.Parameter.In == p.In && ep.Parameter.Name == p.Name {
					alreadyExists = true

					break
				}
			}

			if alreadyExists {
				return fmt.Errorf("parameter %s in %s is already defined", p.Name, p.In)
			}

			o.Parameters = append(o.Parameters, ParameterOrReference{Parameter: &p})

			return nil
		}),
	)
	if err != nil {
		return err
	}

	if s.AdditionalProperties != nil &&
		s.AdditionalProperties.TypeBoolean != nil &&
		!*s.AdditionalProperties.TypeBoolean {
		o.WithMapOfAnythingItem(xForbidUnknown+string(in), true)
	}

	return nil
}

var defNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9.\-_]+`)

func sanitizeDefName(rc *jsonschema.ReflectContext) {
	jsonschema.InterceptDefName(func(t reflect.Type, defaultDefName string) string {
		return defNameSanitizer.ReplaceAllString(defaultDefName, "")
	})(rc)
}

func (r *Reflector) collectDefinition(namePrefix string) func(name string, schema jsonschema.Schema) {
	return func(name string, schema jsonschema.Schema) {
		name = namePrefix + name

		if _, exists := r.SpecEns().ComponentsEns().Schemas[name]; exists {
			return
		}

		sm, err := schema.ToSchemaOrBool().ToSimpleMap()
		if err != nil {
			panic("BUG:" + err.Error())
		}

		r.SpecEns().ComponentsEns().WithSchemasItem(name, sm)
	}
}

func (r *Reflector) parseResponseHeader(resp *Response, oc openapi.OperationContext, cu openapi.ContentUnit) error {
	output := cu.Structure
	mapping := cu.FieldMapping(openapi.InHeader)

	if output == nil {
		return nil
	}

	res := make(map[string]HeaderOrReference)

	schema, err := r.Reflect(output,
		openapi.WithOperationCtx(oc, true, openapi.InHeader),
		jsonschema.InlineRefs,
		jsonschema.PropertyNameMapping(mapping),
		jsonschema.PropertyNameTag(tagHeader),
		sanitizeDefName,
		jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
			if !params.Processed || len(params.Path) > 1 { // only top-level fields (including embedded).
				return nil
			}

			propertySchema := params.PropertySchema
			field := params.Field
			name := params.Name

			sm, err := propertySchema.ToSchemaOrBool().ToSimpleMap()
			if err != nil {
				return err
			}

			header := Header{
				Description:   propertySchema.Description,
				Deprecated:    isDeprecated(propertySchema.ToSchemaOrBool()),
				Schema:        sm,
				Content:       nil,
				Example:       nil,
				Examples:      nil,
				MapOfAnything: nil,
			}

			err = refl.PopulateFieldsFromTags(&header, field.Tag)
			if err != nil {
				return err
			}

			res[name] = HeaderOrReference{
				Header: &header,
			}

			return nil
		}),
	)
	if err != nil {
		return err
	}

	resp.Headers = res

	if schema.Description != nil && resp.Description == "" {
		resp.Description = *schema.Description
	}

	return nil
}

func (r *Reflector) hasJSONBody(output interface{}) (bool, error) {
	schema, err := r.Reflect(output, sanitizeDefName)
	if err != nil {
		return false, err
	}

	// Remove non-constraining fields to prepare for marshaling.
	schema.Title = nil
	schema.Description = nil
	schema.Comment = nil
	schema.ExtraProperties = nil
	schema.ID = nil
	schema.Examples = nil

	j, err := json.Marshal(schema)
	if err != nil {
		return false, err
	}

	if !bytes.Equal([]byte("{}"), j) && !bytes.Equal([]byte(`{"type":"object"}`), j) {
		return true, nil
	}

	return false, nil
}

func (r *Reflector) setupResponse(o *Operation, oc openapi.OperationContext) error {
	for _, cu := range oc.Response() {
		if cu.HTTPStatus == 0 && !cu.IsDefault {
			cu.HTTPStatus = http.StatusOK
		}

		cu.ContentType = strings.Split(cu.ContentType, ";")[0]

		httpStatus := strconv.Itoa(cu.HTTPStatus)
		resp := o.ResponsesEns().MapOfResponseOrReferenceValues[httpStatus].Response

		switch {
		case cu.IsDefault:
			httpStatus = "default"

			if o.Responses.Default == nil {
				o.Responses.Default = &ResponseOrReference{}
			}

			resp = o.Responses.Default.Response
		case cu.HTTPStatus > 0 && cu.HTTPStatus < 6:
			httpStatus = strconv.Itoa(cu.HTTPStatus) + "XX"
			resp = o.Responses.MapOfResponseOrReferenceValues[httpStatus].Response
		}

		if resp == nil {
			resp = &Response{}
		}

		if strings.ToUpper(oc.Method()) != http.MethodHead {
			if err := joinErrors(
				r.parseJSONResponse(resp, oc, cu),
				r.parseResponseHeader(resp, oc, cu),
			); err != nil {
				return err
			}

			if cu.ContentType != "" {
				r.ensureResponseContentType(resp, cu.ContentType, cu.Format)
			}
		} else {
			// Only headers with HEAD method.
			if err := r.parseResponseHeader(resp, oc, cu); err != nil {
				return err
			}
		}

		if cu.Description != "" {
			resp.Description = cu.Description
		}

		if resp.Description == "" {
			resp.Description = http.StatusText(cu.HTTPStatus)
		}

		if cu.IsDefault {
			o.Responses.Default = &ResponseOrReference{Response: resp}
		} else {
			o.Responses.WithMapOfResponseOrReferenceValuesItem(httpStatus, ResponseOrReference{Response: resp})
		}
	}

	return nil
}

func (r *Reflector) ensureResponseContentType(resp *Response, contentType string, format string) {
	if _, ok := resp.Content[contentType]; !ok {
		if resp.Content == nil {
			resp.Content = map[string]MediaType{}
		}

		resp.Content[contentType] = mediaType(format)
	}
}

func (r *Reflector) parseJSONResponse(resp *Response, oc openapi.OperationContext, cu openapi.ContentUnit) error {
	output := cu.Structure
	contentType := cu.ContentType

	if output == nil {
		return nil
	}

	// Check if output structure exposes meaningful schema.
	if hasJSONBody, err := r.hasJSONBody(output); err == nil && !hasJSONBody {
		return nil
	}

	schema, err := r.Reflect(output,
		openapi.WithOperationCtx(oc, true, openapi.InBody),
		jsonschema.RootRef,
		jsonschema.DefinitionsPrefix(componentsSchemas),
		jsonschema.CollectDefinitions(r.collectDefinition("")),
		sanitizeDefName,
	)
	if err != nil {
		return err
	}

	sm, err := schema.ToSchemaOrBool().ToSimpleMap()
	if err != nil {
		return err
	}

	if resp.Content == nil {
		resp.Content = map[string]MediaType{}
	}

	if contentType == "" {
		contentType = mimeJSON
	}

	resp.Content[contentType] = MediaType{
		Schema:        sm,
		Example:       nil,
		Examples:      nil,
		Encoding:      nil,
		MapOfAnything: nil,
	}

	if schema.Description != nil && resp.Description == "" {
		resp.Description = *schema.Description
	}

	return nil
}

// SpecSchema returns OpenAPI spec schema.
func (r *Reflector) SpecSchema() openapi.SpecSchema {
	return r.SpecEns()
}

// JSONSchemaReflector provides access to a low-level struct reflector.
func (r *Reflector) JSONSchemaReflector() *jsonschema.Reflector {
	return &r.Reflector
}
