package publish

import (
	"os"
	"strings"
)

// Environment names the action sets for the publisher (.github/actions/release-descriptor/action.yml). Inputs arrive
// only through these variables, never as process arguments or shell text; the trusted context comes from the
// `github` context in the action's own step environment, which a caller's job environment cannot override.
const (
	EnvPipelineFile         = "PC_PIPELINE_FILE"
	EnvBuildOutputsFile     = "PC_BUILD_OUTPUTS_FILE"
	EnvDescriptorRegistry   = "PC_DESCRIPTOR_REGISTRY"
	EnvDescriptorRepository = "PC_DESCRIPTOR_REPOSITORY"
	EnvCredentialFile       = "PC_CREDENTIAL_FILE" //nolint:gosec // G101: an environment variable name, not a credential
	EnvInitializeJournal    = "PC_INITIALIZE_JOURNAL"
	EnvJobToken             = "PC_JOB_TOKEN" //nolint:gosec // G101: an environment variable name, not a credential
	EnvStateDir             = "PC_STATE_DIR"
)

// OptionsFromEnv reads the action's inputs and trusted context. Malformed booleans are input errors.
func OptionsFromEnv(getenv func(string) string) (Options, error) {
	o := Options{
		Context: Context{
			Actions:     getenv("PC_CTX_ACTIONS"),
			ServerURL:   getenv("PC_CTX_SERVER_URL"),
			Repository:  getenv("PC_CTX_REPOSITORY"),
			SHA:         getenv("PC_CTX_SHA"),
			Ref:         getenv("PC_CTX_REF"),
			RefType:     getenv("PC_CTX_REF_TYPE"),
			EventName:   getenv("PC_CTX_EVENT_NAME"),
			WorkflowRef: getenv("PC_CTX_WORKFLOW_REF"),
			RunID:       getenv("PC_CTX_RUN_ID"),
			RunAttempt:  getenv("PC_CTX_RUN_ATTEMPT"),
			Workspace:   getenv("PC_CTX_WORKSPACE"),
			Actor:       getenv("PC_CTX_ACTOR"),
		},
		PipelineFile:         getenv(EnvPipelineFile),
		BuildOutputsFile:     getenv(EnvBuildOutputsFile),
		DescriptorRegistry:   getenv(EnvDescriptorRegistry),
		DescriptorRepository: getenv(EnvDescriptorRepository),
		CredentialFile:       getenv(EnvCredentialFile),
		JobToken:             getenv(EnvJobToken),
		StateDir:             getenv(EnvStateDir),
		OutputFile:           getenv("GITHUB_OUTPUT"),
	}
	switch getenv(EnvInitializeJournal) {
	case "", "false":
	case "true":
		o.InitializeJournal = true
	default:
		return Options{}, fail(CodeInputInvalid, "initialize-journal")
	}
	if o.StateDir == "" {
		o.StateDir = strings.TrimRight(getenv("RUNNER_TEMP"), "/") + "/pc-release-descriptor"
		if getenv("RUNNER_TEMP") == "" {
			o.StateDir = os.TempDir() + "/pc-release-descriptor"
		}
	}
	for _, v := range []string{o.PipelineFile, o.DescriptorRegistry, o.DescriptorRepository} {
		if len(v) > 255 {
			return Options{}, fail(CodeInputInvalid, "")
		}
	}
	return o, nil
}

// CommandEscape escapes a workflow-command message: '%', CR and LF (the runner's data encoding).
func CommandEscape(s string) string {
	return strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A").Replace(s)
}
