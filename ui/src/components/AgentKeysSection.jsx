import { useState } from "react";
import { Alert, Box, Button, Chip, Stack, TextField, Typography } from "@mui/material";
import api from "../api.js";

// The credential each agent framework runs as, set from the same pane
// that picks the framework. They are ordinary secrets, stored under the
// names below, but a bare name and key says nothing about which
// framework it belongs to or whether the one selected above can actually
// run -- so the pair lives here instead, next to the choice that needs
// them. (grain/task-110 finished that move for the rest: every other
// secret grain resolves is now set beside the capability that resolves
// it, and there is no secrets pane of its own left to set one from.)
//
// Write-only, like everything else built on the secrets store: a value
// goes in, presence comes back, and no value is ever read out.
const FRAMEWORKS = [
  {
    id: "antigravity",
    label: "Gemini API key",
    secret: "gemini-api-key",
    setFlag: "geminiApiKeySet",
    help: "the key the Antigravity CLI (agy) authenticates with -- stored as the \"gemini-api-key\" secret",
  },
  {
    id: "claude",
    label: "Claude Code OAuth token",
    secret: "claude-oauth-token",
    setFlag: "claudeOAuthTokenSet",
    help: "passed to the claude CLI as CLAUDE_CODE_OAUTH_TOKEN -- stored as the \"claude-oauth-token\" secret",
  },
  {
    id: "codex",
    label: "OpenAI API key",
    secret: "openai-api-key",
    setFlag: "openaiApiKeySet",
    help: "passed to the codex CLI as OPENAI_API_KEY -- stored as the \"openai-api-key\" secret",
  },
];

// AGENT_KEY_SECRETS is which secrets this section owns, for
// SettingsOverlay to keep out of the "other secrets" remainder it shows
// on the Capabilities tab -- one list, so a secret cannot be both set
// here and listed there as unclaimed.
export const AGENT_KEY_SECRETS = FRAMEWORKS.map((f) => f.secret);

// AgentKeysSection renders inside SettingsOverlay's own <form>, so every
// control here is a type="button" and Enter is swallowed: a stray submit
// would save the deployment's settings from a field that has nothing to
// do with them.
//
// It is seeded from the Settings response the pane already fetched
// (agentKeysEnabled plus one presence flag per framework) rather than
// fetching presence itself -- one request for one pane -- and every
// mutation below answers with that same shape, which is what keeps the
// chips current without a reload.
export default function AgentKeysSection({ settings, showError }) {
  const [keys, setKeys] = useState({
    enabled: !!settings.agentKeysEnabled,
    geminiApiKeySet: !!settings.geminiApiKeySet,
    claudeOAuthTokenSet: !!settings.claudeOAuthTokenSet,
    openaiApiKeySet: !!settings.openaiApiKeySet,
  });
  const [values, setValues] = useState({});

  // Only the presence flags are taken from a mutation's reply: it
  // answers with the agent-keys shape, whose own "enabled" says the same
  // thing settings.agentKeysEnabled already did (a mutation that reached
  // the store at all proves it), so keeping the seeded value means one
  // less field two different responses have to agree on.
  const applyKeys = (resp) => setKeys((prev) => ({
    ...prev,
    geminiApiKeySet: !!resp.geminiApiKeySet,
    claudeOAuthTokenSet: !!resp.claudeOAuthTokenSet,
    openaiApiKeySet: !!resp.openaiApiKeySet,
  }));

  const setKey = async (id) => {
    try {
      applyKeys(await api(`/api/agent-keys/${id}`, { method: "PUT", body: JSON.stringify({ value: values[id] || "" }) }));
      setValues((prev) => ({ ...prev, [id]: "" }));
    } catch (err) {
      showError(err);
    }
  };

  const clearKey = async (id) => {
    try {
      applyKeys(await api(`/api/agent-keys/${id}`, { method: "DELETE" }));
    } catch (err) {
      showError(err);
    }
  };

  if (!keys.enabled) {
    return (
      <Alert severity="info" sx={{ mb: 2 }}>
        Agent credentials cannot be set from here: this UI has no local secrets directory to write to. A deployment
        seeds them with -gemini-api-key-file / -claude-oauth-token-file / -openai-api-key-file instead.
      </Alert>
    );
  }

  return (
    <Box sx={{ mb: 2 }}>
      {FRAMEWORKS.map((f) => (
        <Box key={f.id} sx={{ mb: 1.5 }}>
          <Stack direction="row" alignItems="center" spacing={1} sx={{ mb: 0.5 }}>
            <Typography variant="body2">{f.label}</Typography>
            <Chip size="small" label={keys[f.setFlag] ? "set" : "not set"} color={keys[f.setFlag] ? "success" : "default"} />
          </Stack>
          <Stack direction="row" spacing={1} alignItems="flex-start">
            <TextField
              type="password"
              size="small"
              fullWidth
              autoComplete="off"
              placeholder={keys[f.setFlag] ? "replace the stored value" : "paste a value to store"}
              helperText={f.help}
              inputProps={{ "aria-label": f.label }}
              value={values[f.id] || ""}
              onChange={(evt) => setValues((prev) => ({ ...prev, [f.id]: evt.target.value }))}
              onKeyDown={(evt) => {
                if (evt.key === "Enter") {
                  evt.preventDefault();
                  if ((values[f.id] || "").trim() !== "") setKey(f.id);
                }
              }}
            />
            <Button
              type="button"
              variant="outlined"
              size="small"
              disabled={(values[f.id] || "").trim() === ""}
              onClick={() => setKey(f.id)}
              sx={{ mt: 0.25 }}
            >
              Set
            </Button>
            <Button
              type="button"
              variant="text"
              size="small"
              disabled={!keys[f.setFlag]}
              onClick={() => clearKey(f.id)}
              sx={{ mt: 0.25 }}
            >
              Clear
            </Button>
          </Stack>
        </Box>
      ))}
      <Typography variant="body2" color="text.secondary">
        Stored on this host and never read back. A key set here takes effect on the next dispatch -- no restart -- and a
        run whose framework has none fails with a note saying so rather than the daemon refusing to start.
      </Typography>
    </Box>
  );
}
