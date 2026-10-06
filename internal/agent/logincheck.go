package agent

import "time"

// ForgetLoginChecks lets the next RefreshClaudeUsage and RefreshCodexLogin ask their provider at once instead of waiting out their ten-minute polls. First-run setup's brain step asks for a fresh check after the user signs in again in a terminal; without this the row went on saying the login had expired for up to ten minutes after it worked again. Input: none. Output: none.
func ForgetLoginChecks() {
	claudeUsagePolled.Lock()
	claudeUsagePolled.at = time.Time{}
	claudeUsagePolled.Unlock()
	codexLoginChecked.Lock()
	codexLoginChecked.at = time.Time{}
	codexLoginChecked.Unlock()
}
