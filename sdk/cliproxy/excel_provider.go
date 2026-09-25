package cliproxy

// excelProviderKey is the provider identifier for the ChatGPT Excel
// (Basispoints) provider. It must match executor.ExcelExecutor.Identifier().
//
// The provider is deliberate fork-specific behaviour: it serves Codex Responses
// clients from the OpenAI backend used by the official Excel add-in, whose
// requests are not subject to the public Codex endpoint's automatic model
// routing. See internal/excel for the protocol implementation.
const excelProviderKey = "excel"
