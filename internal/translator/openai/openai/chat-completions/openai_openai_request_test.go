package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToOpenAI_SanitizesFunctionToolSchema(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"Reply with ok"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"update_plan",
					"description":"update plan",
					"parameters":{
						"type":"object",
						"properties":{
							"plan":{
								"type":"array",
								"items":null
							}
						},
						"required":null
					}
				}
			}
		]
	}`)

	output := ConvertOpenAIRequestToOpenAI("nowcoding/gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if got := gjson.Get(outputStr, "tools.0.function.parameters.properties.plan.items.type").String(); got != "object" {
		t.Fatalf("plan.items.type = %q, want object", got)
	}
	if !gjson.Get(outputStr, "tools.0.function.parameters.required").IsArray() {
		t.Fatal("required should be normalized to an array")
	}
	if got := len(gjson.Get(outputStr, "tools.0.function.parameters.required").Array()); got != 0 {
		t.Fatalf("required length = %d, want 0", got)
	}
}

func TestConvertOpenAIRequestToOpenAI_DoesNotSynthesizePropertiesType(t *testing.T) {
	inputJSON := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"Reply with ok"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"spawn_agent",
					"parameters":{
						"type":"object",
						"properties":{
							"items":{"type":"array","items":null}
						},
						"required":["items","missing"]
					}
				}
			}
		]
	}`)

	output := ConvertOpenAIRequestToOpenAI("nowcoding/gpt-5.4", inputJSON, false)
	outputStr := string(output)

	if gjson.Get(outputStr, "tools.0.function.parameters.properties.type").Exists() {
		t.Fatal("properties.type should not be synthesized from a property named items")
	}
	if got := len(gjson.Get(outputStr, "tools.0.function.parameters.required").Array()); got != 1 {
		t.Fatalf("required length = %d, want 1", got)
	}
}
