FROM python:3.12.12-slim-bookworm@sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c AS base
WORKDIR /app
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 OMP_NUM_THREADS=2
COPY converters/requirements.txt /app/requirements.txt
RUN apt-get update && apt-get install -y --no-install-recommends libcairo2 \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir -r /app/requirements.txt \
 && useradd --uid 10001 --create-home adapter
COPY converters/adapter.py converters/prepare_models.py converters/model-lock.json /app/
USER 10001:10001
EXPOSE 8090
HEALTHCHECK --interval=15s --timeout=3s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz',timeout=2)"
ENTRYPOINT ["python", "/app/adapter.py"]

FROM base AS paddleocr
USER root
RUN apt-get update && apt-get install -y --no-install-recommends libgomp1 libgl1 libglib2.0-0 \
 && rm -rf /var/lib/apt/lists/*
COPY converters/requirements-ocr.txt /app/requirements-ocr.txt
RUN pip install --no-cache-dir -c /app/requirements.txt -r /app/requirements-ocr.txt \
 && mkdir -p /models && chown 10001:10001 /models
ENV ADAPTER_MODE=paddleocr PADDLE_MODELS_DIR=/models PADDLE_PDX_DISABLE_MODEL_SOURCE_CHECK=True ADAPTER_MEMORY_MB=4096
USER 10001:10001
