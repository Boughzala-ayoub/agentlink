package wake

import (
	"os"
	"os/exec"
	"runtime"
)

// Notify shows the human an OS notification; clicking it opens url where the
// OS supports that. It returns without waiting. Tests replace it.
var Notify = func(title, body, url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows": // a WinRT toast from Windows PowerShell 5.1, no module needed
		cmd = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", toastScript)
		cmd.Env = append(os.Environ(), "AGENTLINK_TOAST_TITLE="+title, "AGENTLINK_TOAST_BODY="+body, "AGENTLINK_TOAST_URL="+url)
	case "darwin":
		cmd = exec.Command("osascript", "-e", `on run argv
display notification (item 2 of argv) with title (item 1 of argv)
end run`, title, body)
	default:
		cmd = exec.Command("notify-send", title, body)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// The text arrives in environment variables, so no quoting can break the script.
const toastScript = `
$null = [Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime]
$null = [Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime]
$e = [Security.SecurityElement]
$xml = '<toast activationType="protocol" launch="' + $e::Escape($env:AGENTLINK_TOAST_URL) + '"><visual><binding template="ToastGeneric"><text>' +
  $e::Escape($env:AGENTLINK_TOAST_TITLE) + '</text><text>' + $e::Escape($env:AGENTLINK_TOAST_BODY) + '</text></binding></visual></toast>'
$doc = New-Object Windows.Data.Xml.Dom.XmlDocument
$doc.LoadXml($xml)
$app = '{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe'
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($app).Show([Windows.UI.Notifications.ToastNotification]::new($doc))
`
