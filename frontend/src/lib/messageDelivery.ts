/** Delivery uncertainty is not a refusal and must never invite a blind resend. */
export class MessageDeliveryError extends Error {
  constructor(message: string, readonly deliveryStatus: 'not-delivered' | 'unknown' | 'text-delivered', readonly operationId: string) {
    super(message);
    this.name = 'MessageDeliveryError';
  }
}
