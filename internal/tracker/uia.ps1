# uia.ps1 is the UI Automation host June's daemon keeps running on Windows (see uia_windows.go). PowerShell runs as `-Command -`, reading statements from stdin; the daemon's first line runs this text from base64, so it is never written to disk.
# Protocol: each later stdin line is `UiaServe '<base64 of a JSON request>'`, answered by one JSON line on stdout. Every request carries op, hwnd, ref, text and ms; a failed request answers {"err": "..."}.

$ProgressPreference = 'SilentlyContinue'

# Per-monitor DPI awareness goes first, before UI Automation loads: a DPI-unaware client is handed bounding rectangles scaled to logical pixels, and the daemon works in physical ones.
try {
    Add-Type -Namespace JuneUia -Name Dpi -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr v); [DllImport("user32.dll")] public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr v);'
    [void][JuneUia.Dpi]::SetProcessDpiAwarenessContext([IntPtr](-4))
    [void][JuneUia.Dpi]::SetThreadDpiAwarenessContext([IntPtr](-4))
} catch {}

Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes, WindowsBase, System.Web.Extensions

$js = New-Object System.Web.Script.Serialization.JavaScriptSerializer
$js.MaxJsonLength = [int]::MaxValue

$AE = [System.Windows.Automation.AutomationElement]
# PowerShell variable names ignore case, so no other variable in this script may be named $props in any case, or it overwrites this table.
$Props = @{
    ct  = $AE::ControlTypeProperty
    n   = $AE::NameProperty
    r   = $AE::BoundingRectangleProperty
    off = $AE::IsOffscreenProperty
    pw  = $AE::IsPasswordProperty
    f   = $AE::HasKeyboardFocusProperty
    tg  = $AE::IsTogglePatternAvailableProperty
    tp  = $AE::IsTextPatternAvailableProperty
    rid = $AE::RuntimeIdProperty
    v   = [System.Windows.Automation.ValuePattern]::ValueProperty
    ro  = [System.Windows.Automation.ValuePattern]::IsReadOnlyProperty
}
# One cache request fetches every property above with each step of the walk, instead of one cross-process call per property.
$cr = New-Object System.Windows.Automation.CacheRequest
foreach ($prop in $Props.Values) { $cr.Add($prop) }
$walker = [System.Windows.Automation.TreeWalker]::ControlViewWalker

# els maps a ref to the element it was read from, so acting on a listed element needs no search. It only grows between clears; a ref missing from it is found again by its runtime id (see UiaFind).
$S = @{ els = @{}; nodes = $null; seen = 0; sw = $null; ms = 0; vcap = 500 }
$maxDepth = 40
$maxNodes = 4000
$maxText = 100000
# The control types an observe walk describes, the ones uiaRole in uia.go maps to an actionable role (see act.Actionable); every other element is only walked through, which keeps a large page inside the walk's time budget.
$listed = [System.Collections.Generic.HashSet[int]]::new([int[]](50000, 50002, 50003, 50004, 50005, 50007, 50009, 50011, 50013, 50015, 50016, 50019, 50020, 50024, 50029, 50030, 50031))

# UiaDesc describes one element with the short keys uiaNode in uia.go decodes. Input: the element, whether to read its cached or its live properties, the window handle to build its ref from ($null for none), and the longest value to keep. Output: a hashtable.
# Properties are compared with -eq rather than cast to bool, because an unsupported property comes back as AutomationElement.NotSupported, which casts to true.
function UiaDesc($el, $c, $hw, $vcap) {
    $m = if ($c) { 'GetCachedPropertyValue' } else { 'GetCurrentPropertyValue' }
    $pw = $el.$m($Props.pw) -eq $true
    $v = $el.$m($Props.v)
    if ($pw -or $v -isnot [string]) { $v = '' } elseif ($v.Length -gt $vcap) { $v = $v.Substring(0, $vcap) }
    $h = @{
        c = [int]$el.$m($Props.ct).Id; n = [string]$el.$m($Props.n); v = [string]$v; pw = $pw
        off = $el.$m($Props.off) -eq $true; tg = $el.$m($Props.tg) -eq $true; wr = $el.$m($Props.ro) -eq $false
        f = $el.$m($Props.f) -eq $true; x = 0; y = 0; w = 0; h = 0; id = ''
    }
    # An element with no rectangle reports Rect.Empty, whose corners are infinite and cannot travel as JSON.
    $r = $el.$m($Props.r)
    if ($r -is [System.Windows.Rect] -and -not $r.IsEmpty -and -not [double]::IsInfinity($r.X)) {
        $h.x = [int]$r.X; $h.y = [int]$r.Y; $h.w = [int]$r.Width; $h.h = [int]$r.Height
    }
    if ($null -ne $hw) {
        $rid = $el.$m($Props.rid)
        if ($rid -is [int[]]) { $h.id = [string]$hw + ':' + ($rid -join '.') }
    }
    $h
}

# UiaWalk appends an element and its subtree to $S.nodes depth first (an observe walk only the elements of a listed control type), bounded by depth, node count and the request's time budget, so a huge tree answers with what was read in time rather than not at all. Input: the element (with the cache filled), its depth, whether this is a capture walk, and the window handle. Output: none.
# A capture walk takes an element's whole TextPattern text and does not descend under it: a browser's page gives all of its text in that one call.
function UiaWalk($el, $d, $text, $hw) {
    if ($d -gt $maxDepth -or $S.seen -ge $maxNodes -or $S.sw.ElapsedMilliseconds -ge $S.ms) { return }
    $S.seen++
    $leaf = $false
    # An element whose properties cannot be read, such as one that vanished during the walk, is left out rather than failing the whole read.
    try {
        if ($text -or $listed.Contains($el.GetCachedPropertyValue($Props.ct).Id)) {
            $h = UiaDesc $el $true $hw $S.vcap
            $h.d = $d
            if ($text -and -not $h.pw -and $el.GetCachedPropertyValue($Props.tp) -eq $true) {
                try {
                    $h.t = [string]$el.GetCurrentPattern([System.Windows.Automation.TextPattern]::Pattern).DocumentRange.GetText($maxText)
                    $leaf = $true
                } catch {}
            }
            $S.nodes.Add($h)
            if ($h.id) { $S.els[$h.id] = $el }
        }
    } catch {}
    if ($leaf) { return }
    $kid = $null
    try { $kid = $walker.GetFirstChild($el, $cr) } catch {}
    while ($null -ne $kid) {
        UiaWalk $kid ($d + 1) $text $hw
        if ($S.seen -ge $maxNodes -or $S.sw.ElapsedMilliseconds -ge $S.ms) { return }
        try { $kid = $walker.GetNextSibling($kid, $cr) } catch { $kid = $null }
    }
}

# UiaFind resolves a ref ("hwnd:runtime.id") to its element: from the table when it was listed, else by searching its window for the runtime id, which survives a restart of this host. Output: the element, or a thrown error when it has gone.
function UiaFind($ref) {
    $el = $S.els[$ref]
    if ($null -ne $el) { return $el }
    $i = $ref.IndexOf(':')
    if ($i -lt 1) { throw "not a node ref: $ref" }
    [int[]]$ids = $ref.Substring($i + 1).Split('.')
    $root = $AE::FromHandle([IntPtr][long]$ref.Substring(0, $i))
    $cond = [System.Windows.Automation.PropertyCondition]::new($Props.rid, $ids)
    $el = $root.FindFirst([System.Windows.Automation.TreeScope]::Subtree, $cond)
    if ($null -eq $el) { throw 'the element has gone' }
    $S.els[$ref] = $el
    return $el
}

# UiaHandle answers one request. Input: the decoded request. Output: the reply hashtable.
function UiaHandle($q) {
    $op = [string]$q['op']
    switch ($op) {
        'walk' {
            if ($S.els.Count -gt 20000) { $S.els.Clear() }
            $hw = [long]$q['hwnd']
            $text = [bool]$q['text']
            $S.nodes = New-Object 'System.Collections.Generic.List[object]'
            $S.seen = 0
            $S.ms = [int]$q['ms']
            $S.vcap = 500
            if ($text) { $S.vcap = $maxText }
            $S.sw = [System.Diagnostics.Stopwatch]::StartNew()
            $root = $AE::FromHandle([IntPtr]$hw).GetUpdatedCache($cr)
            UiaWalk $root 0 $text $hw
            return @{ nodes = $S.nodes }
        }
        'desc' {
            $ref = [string]$q['ref']
            $h = UiaDesc (UiaFind $ref) $false $null 500
            $h.id = $ref
            return $h
        }
        'act' {
            # ponytail: Invoke on a Win32 button that opens a modal dialog blocks until the dialog closes, so the daemon times out, restarts this host and falls back to a pointer click on a button that was already pressed; run Invoke on a worker thread if that double press is ever seen.
            $el = UiaFind ([string]$q['ref'])
            $p = $null
            if ($el.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern, [ref]$p)) { $p.Invoke(); return @{ action = 'invoke' } }
            if ($el.TryGetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern, [ref]$p)) { $p.Toggle(); return @{ action = 'toggle' } }
            if ($el.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$p)) { $p.Select(); return @{ action = 'select' } }
            if ($el.TryGetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern, [ref]$p)) {
                $st = $p.Current.ExpandCollapseState
                if ($st -eq [System.Windows.Automation.ExpandCollapseState]::Expanded) { $p.Collapse(); return @{ action = 'collapse' } }
                if ($st -ne [System.Windows.Automation.ExpandCollapseState]::LeafNode) { $p.Expand(); return @{ action = 'expand' } }
            }
            throw 'the element offers no Invoke, Toggle, SelectionItem or ExpandCollapse pattern to fire'
        }
        'scroll' {
            $el = UiaFind ([string]$q['ref'])
            $p = $null
            if (-not $el.TryGetCurrentPattern([System.Windows.Automation.ScrollItemPattern]::Pattern, [ref]$p)) { throw 'the element offers no ScrollItem pattern, so it could not be scrolled into view' }
            $p.ScrollIntoView()
            return @{}
        }
        'focused' {
            # Chromium puts the keyboard focus on an element under the one listed, so the focused element's ancestors are checked too.
            $el = UiaFind ([string]$q['ref'])
            if ($el.Current.HasKeyboardFocus) { return @{ f = $true } }
            $at = $AE::FocusedElement
            $raw = [System.Windows.Automation.TreeWalker]::RawViewWalker
            for ($i = 0; $null -ne $at -and $i -lt $maxDepth; $i++) {
                if ([System.Windows.Automation.Automation]::Compare($at, $el)) { return @{ f = $true } }
                $at = $raw.GetParent($at)
            }
            return @{ f = $false }
        }
        'focus' {
            $f = $AE::FocusedElement
            if ($null -eq $f) { return @{ none = $true } }
            $h = UiaDesc $f $false ([long]$q['hwnd']) 500
            $h.p = [int]$f.Current.ProcessId
            if ($h.id) { $S.els[$h.id] = $f }
            return $h
        }
    }
    throw "unknown op: $op"
}

# UiaServe answers one request. Input: the request JSON, base64-encoded so no quoting matters on the line it arrives on. Output: nothing on the pipeline, which the host would print in the console's code page; the reply goes to $juneOut as UTF-8.
function UiaServe($b64) {
    try {
        $r = UiaHandle ($js.DeserializeObject([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b64))))
    } catch {
        $e = $_.Exception
        while ($null -ne $e.InnerException) { $e = $e.InnerException }
        $r = @{ err = [string]$e.Message }
    }
    $juneOut.WriteLine($js.Serialize($r))
}

$juneOut = [IO.StreamWriter]::new([Console]::OpenStandardOutput(), [Text.UTF8Encoding]::new($false))
$juneOut.AutoFlush = $true
$juneOut.WriteLine('{"ready":true}')
