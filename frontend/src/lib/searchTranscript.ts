import type { HistoryTranscript } from '../api/sessionsd';

export function normalizeTranscriptIndexes(transcript: HistoryTranscript): HistoryTranscript {
  return {
    ...transcript,
    messages: transcript.messages.map((message, index) => ({
      ...message,
      index: Number.isFinite(message.index) ? message.index : index,
      id: message.id || `legacy:${Number.isFinite(message.index) ? message.index : index}`
    }))
  };
}
