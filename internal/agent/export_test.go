package agent

import "google.golang.org/genai"

// ToolDeclarations returns the function declarations the live session exposes, the same list liveTools builds. Gemini's native search tool is not included because it has no declaration to hand a non-live model. The trajectory eval in evals/ uses it to give a text-mode model the identical tool surface the voice session has. Input: none. Output: the declarations, in the order the live session sends them.
func ToolDeclarations() []*genai.FunctionDeclaration {
	tools := liveTools()
	if len(tools) == 0 {
		return nil
	}
	return tools[0].FunctionDeclarations
}
