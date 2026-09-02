import { Box, Chip } from "@mui/material";

// AttachmentLinks renders the files already attached to a task or a
// comment (ui.Attachment's own {id, filename, contentType, size} shape,
// bwsalmon/agents#522) as a row of clickable chips, each linking straight
// at GET /api/tasks/{taskId}/attachments/{id} -- the only place an
// attachment's content is served, so there is nothing for this component
// to fetch ahead of a click.
export default function AttachmentLinks({ taskId, attachments }) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mt: 0.5 }}>
      {attachments.map((a) => (
        <Chip
          key={a.id}
          size="small"
          component="a"
          href={`/api/tasks/${taskId}/attachments/${a.id}`}
          target="_blank"
          rel="noopener noreferrer"
          clickable
          label={a.filename}
        />
      ))}
    </Box>
  );
}
