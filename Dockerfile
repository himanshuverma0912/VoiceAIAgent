FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1
ENV TZ=Asia/Kolkata

# 1. Install uv directly from astral's official image
COPY --from=ghcr.io/astral-sh/uv:latest /uv /uvx /bin/

RUN apt-get update && apt-get install -y \
    tzdata \
    ffmpeg \
    libsndfile1 \
    libportaudio2 \
    libgl1 \
    ca-certificates \
    && ln -snf /usr/share/zoneinfo/$TZ /etc/localtime \
    && echo $TZ > /etc/timezone \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .

# 2. Use uv to install dependencies into the system environment
RUN uv pip install --system --no-cache -r requirements.txt

COPY agent.py .
COPY prompts.py .

EXPOSE 8081

CMD ["python", "agent.py", "start"]