# Reference image: the exact AWS CLI version gs3 must be compatible with.
# Used only by the differential ("compat") tests and for dumping the CLI's
# argument tables. Never shipped.
FROM python:3.11-slim

RUN pip install --no-cache-dir awscli==1.31.13

COPY dump_argtable.py dump_parse.py /usr/local/bin/

ENTRYPOINT ["aws"]
