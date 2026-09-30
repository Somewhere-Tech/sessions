import { useState } from 'react';
import type { DispatchMessage } from '../types';
import type { DeliveryReview } from '../hooks/useDeliveryReview';
import { checkMessageDelivery } from '../api/sessionsd/operations';
import { CopyButton } from './CopyButton';
import '../styles/message-delivery.css';

export function DeliveryReviews({ reviews, machineId, sessionId, clear, acknowledged, notDelivered }: {
  reviews: DeliveryReview[]; machineId?: string; sessionId: string;
  clear: (operationId: string) => boolean;
  acknowledged: (review: DeliveryReview) => void; notDelivered: () => void;
}): JSX.Element {
  return <>{reviews.map((review) => <DeliveryReviewNotice key={review.operationId} review={review}
    machineId={machineId} sessionId={sessionId} reviewed={() => { clear(review.operationId); }}
    settled={(status) => {
      if (!clear(review.operationId)) return;
      if (status === 'accepted') acknowledged(review);
      else notDelivered();
    }} />)}</>;
}

export function MessageDeliveryStatus({ message, restore, remove }: {
  message: DispatchMessage; restore: () => void; remove: () => void;
}): JSX.Element | null {
  if (message.status === 'accepted') return <div className="message-delivery-status" role="status"
    title="Sessions acknowledged this submission. It has not yet appeared in provider history; this does not mean work has completed.">Accepted · waiting for conversation</div>;
  if (message.status !== 'failed') return null;
  return <div className="remote-bubble-status message-delivery-status">
    <span title={message.failureReason}>Older local send · check the conversation</span>
    <button type="button" className="remote-bubble-retry" onClick={restore}>Restore draft</button>
    <button type="button" className="remote-bubble-delete" onClick={remove} title="Remove only this local entry, not provider history.">Remove entry</button>
  </div>;
}

export function DeliveryReviewNotice({ review, machineId, sessionId, settled, reviewed }: {
  review: DeliveryReview; machineId?: string; sessionId: string;
  settled: (status: 'accepted' | 'not-delivered') => void; reviewed: () => void;
}): JSX.Element {
  const [checking, setChecking] = useState(false);
  const [detail, setDetail] = useState('Check the conversation before sending again. Nothing will be resent automatically. Your draft is still here.');
  const check = async (): Promise<void> => {
    if (checking) return;
    setChecking(true);
    try {
      const receipt = await checkMessageDelivery(review.operationId, machineId);
      if (receipt.session_id !== sessionId) throw new Error('The receipt belongs to another session. Check this conversation before sending again.');
      if (receipt.status === 'accepted' || (receipt.status === 'not-delivered' && receipt.retry)) settled(receipt.status);
      else setDetail('The receipt still cannot confirm a complete message. Check the conversation before choosing to send again.');
    } catch (error) { setDetail(error instanceof Error ? error.message : 'The receipt is unavailable. Check the conversation before sending again.'); }
    finally { setChecking(false); }
  };
  return <div className="input-composer-notice is-info delivery-review" role="alert">
    <div><strong>{review.status === 'text-delivered' ? 'Text delivered · Enter not confirmed' : 'Delivery not confirmed'}</strong>
      <span className="delivery-review-preview">“{review.text.slice(0, 100)}{review.text.length > 100 ? '…' : ''}”</span><span>{detail}</span>
      <details><summary>Message to review</summary><p className="delivery-review-text">{review.text}</p></details></div>
    <div className="input-composer-notice-actions">
      <CopyButton getText={review.text} label="Copy message" />
      <button type="button" className="btn btn-secondary" disabled={checking} onClick={() => void check()}>{checking ? 'Checking…' : 'Check delivery'}</button>
      <button type="button" className="btn btn-ghost" disabled={checking} onClick={reviewed}
        title="After inspecting the conversation, enable a new intentional send. Sending again may duplicate a message that already arrived.">I've checked — enable send</button>
    </div>
  </div>;
}
