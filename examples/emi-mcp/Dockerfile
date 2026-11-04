# Demo EMI loan MCP server (made-up data). EMI_MCP_ROLE=core or helpdesk; see emi_mcp.py.
FROM python:3.12-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY emi_mcp.py .
USER nobody
EXPOSE 8000
CMD ["python", "emi_mcp.py"]
