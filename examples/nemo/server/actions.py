"""
Custom NeMo Guardrails action: content safety via L0 Bouncer classifier.

Loads the L0 Bouncer model (22M params, DeBERTa-v3-xsmall) at import time
and runs inference directly inside the NeMo process — no LLM, no vLLM sidecar.

Latency: ~5ms per request on CPU.
"""

import asyncio
import time

import torch
from nemoguardrails.actions import action
from transformers import AutoModelForSequenceClassification, AutoTokenizer

MODEL_NAME = "vincentoh/deberta-v3-xsmall-l0-bouncer"
UNSAFE_THRESHOLD = 0.5

print(f"[classifier-guard] Loading model: {MODEL_NAME}")
_tokenizer = AutoTokenizer.from_pretrained(MODEL_NAME)
_model = AutoModelForSequenceClassification.from_pretrained(MODEL_NAME)
_model.eval()
print("[classifier-guard] Model loaded.")


def _infer_unsafe_prob(text: str) -> float:
    """Run tokenization + model inference (CPU-bound, called via to_thread)."""
    inputs = _tokenizer(text, return_tensors="pt", truncation=True, max_length=512)
    with torch.no_grad():
        logits = _model(**inputs).logits
    probs = torch.softmax(logits, dim=-1)
    return probs[0][1].item()


@action(is_system_action=True)
async def check_content_safety(context: dict | None = None):
    """
    Return True if the message is safe, False if unsafe.
    NeMo passes context automatically when is_system_action=True.
    """
    text = context.get("user_message", "") if context else ""
    if not text:
        return True

    start = time.perf_counter_ns()
    unsafe_prob = await asyncio.to_thread(_infer_unsafe_prob, text)
    elapsed_ms = (time.perf_counter_ns() - start) / 1_000_000

    is_safe = unsafe_prob <= UNSAFE_THRESHOLD

    print(
        f"[classifier-guard] [{elapsed_ms:.1f}ms] "
        f"text_len={len(text)} unsafe={unsafe_prob:.3f} safe={is_safe}"
    )
    return is_safe
