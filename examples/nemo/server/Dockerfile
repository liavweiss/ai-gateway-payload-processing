# NeMo Guardrails with in-process classifier guard.
# Single container: NeMo server + custom Python action that loads a DeBERTa
# safety classifier (L0 Bouncer, 22M params) and runs inference on CPU.
# No GPU, no sidecar, no external model service.
# Expected image size: ~1.5 GB  |  Expected latency: ~5-10ms per request.
FROM python:3.11-slim

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends \
    g++ gcc python3-dev && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir torch==2.7.0 --index-url https://download.pytorch.org/whl/cpu

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Pre-bake the L0 Bouncer model so the container works offline in K8s.
RUN python -c "\
from transformers import AutoModelForSequenceClassification, AutoTokenizer; \
AutoTokenizer.from_pretrained('vincentoh/deberta-v3-xsmall-l0-bouncer'); \
AutoModelForSequenceClassification.from_pretrained('vincentoh/deberta-v3-xsmall-l0-bouncer'); \
print('L0 Bouncer model cached.')"

COPY config.yml config.co actions.py /configs/config/

RUN useradd -u 10001 -m appuser && \
    chown -R appuser:appuser /app /configs
USER appuser

EXPOSE 8000

CMD ["nemoguardrails", "server", "--config", "/configs", "--default-config-id", "config", "--port", "8000"]
