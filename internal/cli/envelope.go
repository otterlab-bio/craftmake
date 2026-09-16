package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/otterlab-bio/craftmake/pkg/protocol"
)

type outputFormat string

const (
	outputFormatText  outputFormat = "text"
	outputFormatJSON  outputFormat = "json"
	outputFormatJSONL outputFormat = "jsonl"
)

func parseOutputFormat(value string) (outputFormat, error) {
	switch outputFormat(value) {
	case outputFormatText, outputFormatJSON, outputFormatJSONL:
		return outputFormat(value), nil
	default:
		return "", usageError("unsupported output format %q; expected text, json, or jsonl", value)
	}
}

func writeCommandOutput(
	command *cobra.Command,
	formatValue string,
	envelope protocol.CommandEnvelope,
	writeText func() error,
) error {
	format, err := parseOutputFormat(formatValue)
	if err != nil {
		return err
	}
	switch format {
	case outputFormatText:
		return writeText()
	case outputFormatJSON:
		return protocol.WriteCommandEnvelope(command.OutOrStdout(), envelope, true)
	case outputFormatJSONL:
		return protocol.WriteCommandEnvelope(command.OutOrStdout(), envelope, false)
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}
