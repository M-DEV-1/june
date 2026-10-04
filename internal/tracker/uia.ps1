# uia.ps1 is the UI Automation host June's daemon keeps running on Windows (see uia_windows.go). PowerShell runs as `-Command -`, reading statements from stdin; the daemon's first line runs this text from base64, so it is never written to disk.
# Protocol: each later stdin line is `UiaServe '<base64 of a JSON request>'`, answered by one JSON line on stdout. Every request carries op, hwnd, ref, text and ms (how long a walk may read, or an act may take to find its element and wait for its action to return); a failed request answers {"err": "..."}.

$ProgressPreference = 'SilentlyContinue'

# Per-monitor DPI awareness goes first, before UI Automation loads: a DPI-unaware client is handed bounding rectangles scaled to logical pixels, and the daemon works in physical ones.
try {
    Add-Type -Namespace JuneUia -Name Dpi -MemberDefinition '[DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(IntPtr v); [DllImport("user32.dll")] public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr v);'
    [void][JuneUia.Dpi]::SetProcessDpiAwarenessContext([IntPtr](-4))
    [void][JuneUia.Dpi]::SetThreadDpiAwarenessContext([IntPtr](-4))
} catch {}

Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes, WindowsBase, System.Web.Extensions

# Classic Win32 and WinForms controls (BUTTON, STATIC, Edit, SysTreeView32 and the rest, in a Run dialog, a message box, Explorer's navigation pane) have no UI Automation provider of their own; the client-side proxies that give them one live in UIAutomationClientsideProviders, which the managed client fails to load by itself inside powershell.exe. Without them every such control read as an unnamed-role Pane, which an observe walk does not list and a capture treats as furniture, so those windows read as empty.
# The first registration in a fresh process throws a NullReferenceException and the second succeeds (measured on Windows 11 26200), so it is tried a few times; a host that cannot register still reads every application that has its own provider.
# A host that could not is said so on stderr, which the daemon logs (see uiaStderr in uia_windows.go): the classic windows reading empty again is otherwise indistinguishable in june.log from windows that publish nothing.
$proxyErr = 'it was not tried'
try {
    Add-Type -AssemblyName UIAutomationClientsideProviders -ErrorAction Stop
    foreach ($attempt in 1..3) {
        try { [System.Windows.Automation.ClientSettings]::RegisterClientSideProviders([UIAutomationClientsideProviders.UIAutomationClientSideProviders]::ClientSideProviderDescriptionTable); $proxyErr = $null; break } catch { $proxyErr = $_.Exception.Message }
    }
} catch { $proxyErr = $_.Exception.Message }
if ($null -ne $proxyErr) { [Console]::Error.WriteLine("the Win32 client-side providers could not be registered, so classic Win32 and WinForms windows read as empty: $proxyErr") }

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
# The least of its ms an act must have left once its element is found to be fired at all, the same floor uiaMinBudget in uia_windows.go puts on sending one: with less, an ordinary action would have too little time to return and would be answered as not returned.
$minFireMS = 200
# How long a walk that found a Chromium window's page empty waits before reading it again (see the walk op), and the least of its budget that must be left after the wait for the second read to be worth making.
$pageWaitMS = 700
$pageReadMS = 300
# The control types an observe walk describes, the ones uiaRole in uia.go maps to an actionable role (see act.Actionable); every other element is only walked through, which keeps a large page inside the walk's time budget.
$listed = [System.Collections.Generic.HashSet[int]]::new([int[]](50000, 50002, 50003, 50004, 50005, 50007, 50009, 50011, 50013, 50015, 50016, 50019, 50020, 50024, 50029, 50030, 50031))

# UiaDesc describes one element with the short keys uiaNode in uia.go decodes. Input: the element, whether to read its cached or its live properties, the window handle to build its ref from ($null for none), and the longest value to keep. Output: a hashtable.
# Callers cast the result to [hashtable]: a function's output arrives wrapped in a PSObject, and JavaScriptSerializer walking that wrapper's members fails with a circular reference.
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

# UiaWalk appends an element and its subtree to $S.nodes depth first (an observe walk only the elements of a listed control type), bounded by depth, node count and the request's time budget, so a huge tree answers with what was read in time rather than not at all. Input: the element (with the cache filled), its depth, whether this is a capture walk, the window handle, and its parent's control type (0 for the window). Output: none.
# A capture walk takes an element's whole TextPattern text and does not descend under it: a browser's page gives all of its text in that one call.
function UiaWalk($el, $d, $text, $hw, $pct) {
    if ($d -gt $maxDepth -or $S.seen -ge $maxNodes -or $S.sw.ElapsedMilliseconds -ge $S.ms) { return }
    $S.seen++
    $leaf = $false
    $ct = 0
    try { $ct = $el.GetCachedPropertyValue($Props.ct).Id } catch {}
    # A title bar is the window's frame, not its content. A capture leaves all of it out: its value and its buttons are the window's title over again ("Minimize Calculator", "Close Calculator") and put "System Minimize Calculator Maximize Calculator Close Calculator" at the head of every capture of a store app, which is most of what a short capture's embedding was made of. An observe walk keeps the Minimize, Maximize and Close buttons, which can be pressed and which say a window publishes nothing else, but not the window menu (the menu bar named "System"), which the client-side proxies add to every classic window.
    if ($ct -eq 50037 -and $text) { return }
    if ($ct -eq 50010 -and $pct -eq 50037) { return }
    # A store app's frame is not a TitleBar at all: ApplicationFrameHost draws it as a Window holding the "System" menu bar and buttons named "Minimize Calculator" and so on, so the two lines above never fire for it and every capture of a store app still opened with that caption (measured on Calculator, 2026-10-03). A capture leaves those out by name; an observe walk keeps the buttons, which can be pressed.
    if ($text -and ($ct -eq 50010 -or $ct -eq 50000)) {
        $nm = ''
        try { $nm = [string]$el.GetCachedPropertyValue($Props.n) } catch {}
        if (($ct -eq 50010 -and $nm -eq 'System') -or ($ct -eq 50000 -and $nm -match '^(Minimize|Maximize|Restore|Close) ')) { return }
    }
    # A tooltip is the name of whatever the pointer happens to rest on, shown again for a moment: in a listing it is a line of text that cannot be acted on, and in a capture words the window does not hold.
    if ($ct -eq 50022) { return }
    # A scroll bar is furniture as well. The client-side proxies give every classic one (Explorer's navigation pane, a classic list or edit) four buttons, "Back by small amount", "Back by large amount", "Forward by large amount" and "Forward by small amount", and a thumb: four lines of a listing per scroll bar that are controls, so they outlast page text at the listing's cap, for what the model's own scroll tools already do, and four phrases in every capture of the window.
    if ($ct -eq 50014) { return }
    # An element whose properties cannot be read, such as one that vanished during the walk, is left out rather than failing the whole read.
    try {
        if ($text -or $listed.Contains($ct)) {
            $h = [hashtable](UiaDesc $el $true $hw $S.vcap)
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
        UiaWalk $kid ($d + 1) $text $hw $ct
        if ($S.seen -ge $maxNodes -or $S.sw.ElapsedMilliseconds -ge $S.ms) { return }
        try { $kid = $walker.GetNextSibling($kid, $cr) } catch { $kid = $null }
    }
}

# UiaHasPage reports whether the walk in $S.nodes reached a page: a Document with text of its own or an element under it. Output: a bool.
function UiaHasPage {
    $n = $S.nodes
    for ($i = 0; $i -lt $n.Count; $i++) {
        if ($n[$i].c -ne 50030) { continue }
        if ($n[$i].t -or ($i + 1 -lt $n.Count -and $n[$i + 1].d -gt $n[$i].d)) { return $true }
    }
    return $false
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

# UiaFire calls one of a pattern's action methods (Invoke, Toggle, Select, Expand or Collapse) on a worker thread and waits up to $ms for it. Input: the pattern, the method's name and the wait. Output: $true when the call returned, $false when it is still running; a thrown error when it failed.
# A provider whose action opens a modal dialog before returning, which the UI Automation documentation warns of for Invoke, keeps the call from returning until the dialog is closed. Called on this thread it held the host until the daemon gave up on it, killed it and pressed the control a second time with the pointer; on a worker the press goes out, this host stays free to read the dialog, and the reply says it is still running so the daemon does not press again.
# The delegate is bound to the pattern's own .NET method rather than made from a script block, because a script block needs a runspace and a pool thread has none.
function UiaFire($p, $verb, $ms) {
    $t = [System.Threading.Tasks.Task]::Run([Delegate]::CreateDelegate([Action], $p, $verb))
    return $t.Wait($ms)
}

# UiaHandle answers one request. Input: the decoded request. Output: the reply hashtable.
function UiaHandle($q) {
    $op = [string]$q['op']
    switch ($op) {
        'walk' {
            if ($S.els.Count -gt 20000) { $S.els.Clear() }
            $hw = [long]$q['hwnd']
            $text = [bool]$q['text']
            # ::new() rather than New-Object, whose output arrives wrapped in a PSObject that JavaScriptSerializer cannot serialize.
            $S.nodes = [System.Collections.Generic.List[object]]::new()
            $S.seen = 0
            $S.ms = [int]$q['ms']
            $S.vcap = 500
            if ($text) { $S.vcap = $maxText }
            $S.sw = [System.Diagnostics.Stopwatch]::StartNew()
            $root = $AE::FromHandle([IntPtr]$hw).GetUpdatedCache($cr)
            UiaWalk $root 0 $text $hw 0
            # Chromium (Chrome, Edge and every Electron app) builds a page's accessibility tree only once a client has asked for it, and fills it in over the following second, so the first read of a window nobody had read before saw the browser's frame and toolbar and none of the page. When the page came back empty and the budget leaves room, the window is read once more after a pause.
            if (-not (UiaHasPage) -and $S.ms - $S.sw.ElapsedMilliseconds -ge $pageWaitMS + $pageReadMS -and [string]$root.Current.ClassName -eq 'Chrome_WidgetWin_1') {
                Start-Sleep -Milliseconds $pageWaitMS
                $S.nodes = [System.Collections.Generic.List[object]]::new()
                $S.seen = 0
                UiaWalk ($AE::FromHandle([IntPtr]$hw).GetUpdatedCache($cr)) 0 $text $hw 0
            }
            return @{ nodes = $S.nodes.ToArray() }
        }
        'desc' {
            $ref = [string]$q['ref']
            $h = [hashtable](UiaDesc (UiaFind $ref) $false $null 500)
            $h.id = $ref
            return $h
        }
        'act' {
            # The request's ms is the whole act's, finding the element included. A ref this host never listed, because it was restarted since the list was read, is searched for by runtime id, which can take seconds on a browser's tree; an act still searching when the daemon's deadline passed was killed there, and a host killed mid-act has to be reported as maybe pressed, so nothing pressed the control at all. Refusing once the time is spent answers with a plain error before anything is fired, which leaves the pointer to press it.
            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            $ms = [int]$q['ms']
            $el = UiaFind ([string]$q['ref'])
            $p = $null
            $verb = $null
            if ($el.TryGetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern, [ref]$p)) { $verb = 'Invoke' }
            elseif ($el.TryGetCurrentPattern([System.Windows.Automation.TogglePattern]::Pattern, [ref]$p)) { $verb = 'Toggle' }
            elseif ($el.TryGetCurrentPattern([System.Windows.Automation.SelectionItemPattern]::Pattern, [ref]$p)) { $verb = 'Select' }
            elseif ($el.TryGetCurrentPattern([System.Windows.Automation.ExpandCollapsePattern]::Pattern, [ref]$p)) {
                $st = $p.Current.ExpandCollapseState
                if ($st -eq [System.Windows.Automation.ExpandCollapseState]::Expanded) { $verb = 'Collapse' }
                elseif ($st -ne [System.Windows.Automation.ExpandCollapseState]::LeafNode) { $verb = 'Expand' }
            }
            if ($null -eq $verb) { throw 'the element offers no Invoke, Toggle, SelectionItem or ExpandCollapse pattern to fire' }
            $left = [int]($ms - $sw.ElapsedMilliseconds)
            if ($left -lt $minFireMS) { throw "finding the element took the time this action had, so it was not fired" }
            # pending says the action was fired and had not returned in what was left of the request's ms (see UiaFire).
            return @{ action = $verb.ToLower(); pending = -not (UiaFire $p $verb $left) }
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
            # A read for a field_holds check (text set) takes the value whole, up to the capture's own cap, since text typed at the end of a page already longer than an observe line's 500 characters was cut off and never found. The read the stop lines make before every keystroke needs none of it and keeps the 500.
            $text = [bool]$q['text']
            $vcap = 500
            if ($text) { $vcap = $maxText }
            $h = [hashtable](UiaDesc $f $false ([long]$q['hwnd']) $vcap)
            # A control that publishes its contents through TextPattern alone (Word, Windows Terminal, some rich edits) has no value to read, so the check reads its document text instead.
            if ($text -and -not $h.pw -and $f.GetCurrentPropertyValue($Props.v) -isnot [string] -and $f.GetCurrentPropertyValue($Props.tp) -eq $true) {
                try { $h.t = [string]$f.GetCurrentPattern([System.Windows.Automation.TextPattern]::Pattern).DocumentRange.GetText($maxText) } catch {}
            }
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
    # $r holds the function's output, which PowerShell wraps in a PSObject; serializing the wrapper walks its reflection members and fails with a circular reference, so the base object is what goes out.
    $juneOut.WriteLine($js.Serialize($r.psobject.BaseObject))
}

$juneOut = [IO.StreamWriter]::new([Console]::OpenStandardOutput(), [Text.UTF8Encoding]::new($false))
$juneOut.AutoFlush = $true
$juneOut.WriteLine('{"ready":true}')
