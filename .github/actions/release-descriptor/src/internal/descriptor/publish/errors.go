package publish

import (
	"errors"
	"fmt"
)

// Fixed failure codes of docs/design/23-release-descriptors.md §23.4. A failure carries one code and, at most, a schema
// path or a fixed step name; it never carries submitted file content, credentials or values.
const (
	CodeInputInvalid         = "inputInvalid"
	CodeContextUntrusted     = "contextUntrusted"
	CodeBranchNotConfigured  = "branchNotConfigured"
	CodeFileInvalid          = "fileInvalid"
	CodePipelineInvalid      = "pipelineInvalid"
	CodePipelineMismatch     = "pipelineMismatch"
	CodeRegistryOverride     = "registryOverride"
	CodeBuildOutputsInvalid  = "buildOutputsInvalid"
	CodeRunMismatch          = "runMismatch"
	CodeBuildIncomplete      = "buildIncomplete"
	CodeImageSetMismatch     = "imageSetMismatch"
	CodeTagPatternMismatch   = "tagPatternMismatch"
	CodeImageMissing         = "imageMissing"
	CodeImageInvalid         = "imageInvalid"
	CodeRevisionMismatch     = "revisionMismatch"
	CodeCredentialInvalid    = "credentialInvalid" //nolint:gosec // G101: a failure code, not a credential
	CodeWriteAccessDenied    = "writeAccessDenied"
	CodeRegistryUnavailable  = "registryUnavailable"
	CodeRegistryInvalid      = "registryResponseInvalid"
	CodeJournalHeadMissing   = "journalHeadMissing"
	CodeIndexConflict        = "publicationIndexConflict"
	CodePublicationConflict  = "publicationConflict"
	CodeJournalOverflow      = "journalOverflow"
	CodeAmbiguousPublication = "ambiguousPublication"
	CodeStateInvalid         = "stateInvalid"
	CodeOutputInvalid        = "outputInvalid"
)

// Failure is a fixed, redacted publication failure.
type Failure struct {
	Code   string
	Detail string // a schema path or fixed step name, never input
	cause  error  // kept for errors.Is/As in tests; never printed
}

func (f *Failure) Error() string {
	if f.Detail == "" {
		return f.Code
	}
	return f.Code + " (" + f.Detail + ")"
}

func (f *Failure) Unwrap() error { return f.cause }

func fail(code, detail string) error { return &Failure{Code: code, Detail: detail} }

func failWith(code, detail string, cause error) error {
	return &Failure{Code: code, Detail: detail, cause: cause}
}

// CodeOf returns the fixed code of err, or CodeRegistryUnavailable for an error that is not a Failure, so that a
// caller can always report a fixed code.
func CodeOf(err error) string {
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return CodeRegistryUnavailable
}

// Message is the single line printed for err: the fixed code and its fixed detail.
func Message(err error) string {
	var f *Failure
	if !errors.As(err, &f) {
		return "release-descriptor: " + CodeRegistryUnavailable
	}
	return fmt.Sprintf("release-descriptor: %s", f.Error())
}
