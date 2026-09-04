import { useRef } from "react";
import AttachFileIcon from "@mui/icons-material/AttachFile";
import { Box, Chip, IconButton, Tooltip } from "@mui/material";

// AttachmentPicker is the file-attach control NewTaskOverlay and the
// Timeline's own reply box both need (bwsalmon/agents#522): a paperclip
// button that opens the browser's file picker, a chip per file already
// chosen (with its own remove button), and onChange(files) -- plain File
// objects, not yet read -- so a caller decides when to actually convert
// them (on submit, via fileToAttachment) rather than paying to read one
// that might still be removed before Send is ever clicked.
export default function AttachmentPicker({ files, onChange }) {
  const inputRef = useRef(null);

  const add = (evt) => {
    const picked = Array.from(evt.target.files || []);
    onChange([...files, ...picked]);
    // Lets the same file be picked again after being removed below --
    // otherwise a browser treats an unchanged <input> value as no change
    // at all and never fires onChange a second time for it.
    evt.target.value = "";
  };
  const remove = (index) => onChange(files.filter((_, i) => i !== index));

  return (
    <Box sx={{ mt: 1 }}>
      {/* The paperclip carries the control's whole accessible name: the
          input behind it is hidden plumbing, clicked through the ref
          rather than by the user, so labelling both would just give
          "Attach files" two elements to mean. */}
      <input ref={inputRef} type="file" multiple hidden onChange={add} />
      <Tooltip title="Attach files">
        <IconButton size="small" aria-label="Attach files" onClick={() => inputRef.current.click()}>
          <AttachFileIcon fontSize="small" />
        </IconButton>
      </Tooltip>
      {files.length > 0 && (
        <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mt: 0.5 }}>
          {files.map((f, i) => (
            <Chip key={`${f.name}-${i}`} size="small" label={f.name} onDelete={() => remove(i)} />
          ))}
        </Box>
      )}
    </Box>
  );
}
