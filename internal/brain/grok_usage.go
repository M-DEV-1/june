// grok_usage.go says, in the same voice /brains uses for a limits_note, what was actually checked on this machine: `grok -p --output-format json` answers only {text, stopReason} (see GrokCLI in cli.go), ~/.grok holds no usage, quota, or rate-limit file or field in its config, logs, session, or model-cache files, and there is no `grok usage` or similar subcommand (`grok --help` lists none). The only place a used-fraction for a Grok account exists is a private xAI endpoint reachable only over the network, which this daemon never calls on its own, so there is nothing local to parse into a UsageLimit.
package brain

// GrokNote is the sentence GET /brains and GET /usage carry as the Grok row's limits_note, so the picker's empty bar reads as a checked fact rather than a gap nobody looked at.
func GrokNote() string {
	return "grok exposes no usage data: its CLI, config, logs, and session files carry no quota, usage, or rate-limit reading, and it has no command that reports one"
}
