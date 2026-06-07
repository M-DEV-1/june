Add-Type -AssemblyName UIAutomationClient,UIAutomationTypes,WindowsBase

$focused = [System.Windows.Automation.AutomationElement]::FocusedElement
if ($null -eq $focused) { exit 0 }

# walk up to the parent window
$walker = [System.Windows.Automation.TreeWalker]::ControlViewWalker
$root = $focused
try {
    while ($true) {
        $parent = $walker.GetParent($root)
        if ($null -eq $parent) { break }
        if ($parent.Current.ControlType -eq [System.Windows.Automation.ControlType]::Window) {
            $root = $parent
            break
        }
        $root = $parent
    }
} catch { exit 0 }

$lines = [System.Collections.Generic.List[string]]::new()
$seen  = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
$cap   = 50 * 1024   # 50 KB hard cap

$textPatternId  = [System.Windows.Automation.TextPattern]::Pattern
$valuePatternId = [System.Windows.Automation.ValuePattern]::Pattern

$contentWalker = [System.Windows.Automation.TreeWalker]::ContentViewWalker

function Walk($el) {
    if ($null -eq $el) { return }

    # TextPattern takes priority — gives full document text
    try {
        $tp = $el.GetCurrentPattern($textPatternId)
        if ($null -ne $tp) {
            $t = $tp.DocumentRange.GetText(-1).Trim()
            if ($t.Length -gt 0 -and $seen.Add($t)) {
                $script:lines.Add($t)
            }
        }
    } catch {}

    # ValuePattern for input fields / editors
    try {
        $vp = $el.GetCurrentPattern($valuePatternId)
        if ($null -ne $vp) {
            $t = $vp.Current.Value.Trim()
            if ($t.Length -gt 0 -and $seen.Add($t)) {
                $script:lines.Add($t)
            }
        }
    } catch {}

    # Name fallback when no pattern matched
    try {
        $name = $el.Current.Name.Trim()
        if ($name.Length -gt 0 -and $seen.Add($name)) {
            $script:lines.Add($name)
        }
    } catch {}

    # recurse into children via ContentViewWalker
    try {
        $child = $script:contentWalker.GetFirstChild($el)
        while ($null -ne $child) {
            Walk $child
            if (($script:lines | Measure-Object -Property Length -Sum).Sum -ge $script:cap) { return }
            $child = $script:contentWalker.GetNextSibling($child)
        }
    } catch {}
}

try {
    Walk $root
} catch { exit 0 }

$out = ($lines -join "`n").Substring(0, [Math]::Min(($lines -join "`n").Length, $cap))
Write-Output $out
