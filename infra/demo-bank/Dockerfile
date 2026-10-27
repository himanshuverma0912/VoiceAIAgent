# Demo core-banking server (made-up data) for test calls. See server.py.
FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY server.py .
USER nobody
EXPOSE 8000
CMD ["python", "server.py"]
