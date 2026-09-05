import { useEffect, useRef, useState } from "react";
import { Alert, Box, Button, TextField, Typography } from "@mui/material";
import api from "../api.js";

// RootShellPage is the Root shell tab of DebugOverlay.jsx: one command,
// run as root on the machine the daemon itself runs on, over POST
// /api/host/shell (grain/task-13).
//
// It is the last tab for the same reason it is the last resort. Logs,
// Sandbox health and Top each answer one fixed question well, and the
// deployments worth opening this pane for are the ones where none of
// those three explained anything -- a disk that filled with something no
// panel counts, a unit that will not come back, a container the daemon
// cannot see past. The operator's usual answer to that is an SSH session
// on the host, and the whole point of this tab is the day that is not
// available: a machine that stopped accepting connections, or one in a
// cloud project whose console is somebody else's to reach.
//
// What is on the other end is root, unrestricted, which is more than
// every other control in this UI put together -- hence the standing
// warning above the prompt rather than a confirm on each command. A
// confirm per command would be clicked through by the second one and
// would make the pane useless for the thing it is actually used for,
// which is a dozen short commands in a row while chasing something down.
//
// GET /api/config's own rootShellEnabled flag says whether this
// deployment has a responder behind that route at all; when it does not
// (GRAIN_ROOT_SHELL=0, a host deployed before the responder existed,
// `grain demo`) this shows a note rather than a prompt whose every
// command could only 404 -- the same convention LogsPage and TopPage
// already follow for their own optional halves.
export default function RootShellPage({ config, showError }) {
  const [command, setCommand] = useState("");
  const [running, setRunning] = useState(false);
  // Everything run in this pane since it was opened, oldest first: the
  // command, then whatever it printed. A shell without its scrollback
  // loses the comparison every one of these commands is for -- what the
  // file looked like before the last one changed it.
  const [history, setHistory] = useState([]);
  const scrollback = useRef(null);

  const enabled = Boolean(config && config.rootShellEnabled);

  // Follow the tail, the way a terminal does: a long command's output
  // otherwise lands entirely below the fold.
  useEffect(() => {
    const view = scrollback.current;
    if (view) view.scrollTop = view.scrollHeight;
  }, [history, running]);

  const run = async (event) => {
    event.preventDefault();
    const sent = command.trim();
    if (!sent || running) return;
    setRunning(true);
    try {
      const res = await api("/api/host/shell", {
        method: "POST",
        body: JSON.stringify({ command: sent }),
      });
      setHistory((h) => [
        ...h,
        { command: sent, output: res.output || "", exitCode: res.exitCode },
      ]);
      setCommand("");
    } catch (err) {
      // The command failing is not this branch -- that comes back as a
      // 200 with an exit code, and belongs in the scrollback with the
      // rest. This is the exchange itself failing (no responder on the
      // host, an unwritable control directory), which is a fact about
      // the deployment and goes to the error banner, with the command
      // left in the box to try again after it is fixed.
      showError(err);
    } finally {
      setRunning(false);
    }
  };

  return (
    <section className="root-shell-panel">
      <Typography variant="subtitle2" sx={{ mb: 1 }}>
        Root shell
      </Typography>
      {!enabled && (
        <Alert severity="info" sx={{ mb: 2 }}>
          Not available: this deployment has no root shell configured. It is
          installed by scripts/setup.sh unless GRAIN_ROOT_SHELL=0; a host
          deployed before that existed gets one by re-running the installer.
        </Alert>
      )}
      {enabled && (
        <>
          <Alert severity="warning" sx={{ mb: 2 }}>
            Every command here runs as <strong>root</strong>, on the machine
            this daemon runs on, with no confirmation and no undo. Each one is
            its own shell — write <code>cd /var/lib/grain &amp;&amp; ls</code>
            {" rather than expecting a "}
            <code>cd</code> to stick — and each one is logged to the daemon’s
            journal.
          </Alert>
          <Box
            component="form"
            onSubmit={run}
            className="logs-toolbar"
            sx={{ alignItems: "flex-start" }}
          >
            <TextField
              size="small"
              fullWidth
              autoComplete="off"
              label="Command"
              placeholder="systemctl status grain-daemon.service"
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              disabled={running}
              inputProps={{ spellCheck: "false" }}
            />
            <Button
              type="submit"
              variant="outlined"
              color="error"
              disabled={running || command.trim() === ""}
            >
              {running ? "Running…" : "Run as root"}
            </Button>
          </Box>
          <pre className="logs-view" ref={scrollback}>
            {history.length === 0 && !running
              ? "(nothing run yet)"
              : history
                  .map(
                    (entry) =>
                      `# ${entry.command}\n${entry.output}` +
                      (entry.exitCode ? `[exit ${entry.exitCode}]\n` : ""),
                  )
                  .join("")}
            {running ? `# ${command.trim()}\n…\n` : ""}
          </pre>
        </>
      )}
    </section>
  );
}
