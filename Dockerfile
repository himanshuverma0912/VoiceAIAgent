FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1
ENV TZ=Asia/Kolkata

# 1. Install uv directly from astral's official image
#    (pinned, so a new uv release can't change the build without a commit)
COPY --from=ghcr.io/astral-sh/uv:0.13.5 /uv /uvx /bin/

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

# Health check endpoint of the agent server (GET / returns "OK")
EXPOSE 8081

# livekit-agents 1.8 deprecates `python agent.py start` (cli.run_app);
# this is the replacement. It finds the `server` object in agent.py.
CMD ["python", "-m", "livekit.agents", "start", "agent.py"]