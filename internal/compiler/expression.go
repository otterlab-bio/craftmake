package compiler

import (
	"fmt"
	"regexp"
	"strings"
)

var expressionPattern = regexp.MustCompile(`\$\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}`)
var completeExpressionPattern = regexp.MustCompile(`^\$\{\{\s*([A-Za-z0-9_.-]+)\s*\}\}$`)

func Render(template string, values map[string]any) (string, error) {
	var renderError error
	rendered := expressionPattern.ReplaceAllStringFunc(template, func(expression string) string {
		if renderError != nil {
			return expression
		}
		matches := expressionPattern.FindStringSubmatch(expression)
		value, err := Resolve(values, matches[1])
		if err != nil {
			renderError = err
			return expression
		}
		switch typedValue := value.(type) {
		case string:
			return typedValue
		case fmt.Stringer:
			return typedValue.String()
		case int, int32, int64, float32, float64, bool:
			return fmt.Sprint(typedValue)
		default:
			renderError = fmt.Errorf("expression %q resolves to a non-scalar value", matches[1])
			return expression
		}
	})
	if renderError != nil {
		return "", renderError
	}
	if strings.Contains(rendered, "${{") {
		return "", fmt.Errorf("unresolved expression in %q", template)
	}
	return rendered, nil
}

func Resolve(values map[string]any, path string) (any, error) {
	var current any = values
	for _, segment := range strings.Split(path, ".") {
		currentMap, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot resolve %q at %q", path, segment)
		}
		current, ok = currentMap[segment]
		if !ok {
			return nil, fmt.Errorf("unknown expression path %q", path)
		}
	}
	return current, nil
}

func CompleteExpression(value string) (string, bool) {
	matches := completeExpressionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(matches) != 2 {
		return "", false
	}
	return matches[1], true
}
