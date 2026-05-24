FROM public.ecr.aws/docker/library/python:3.12-slim

WORKDIR /app

RUN apt-get update \
 && apt-get install -y --no-install-recommends rsync \
 && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py snapshotter.py ./

ENV PYTHONUNBUFFERED=1
EXPOSE 8080
CMD ["python", "app.py"]
