# AWS CLI 1.31.13 `aws s3` 동작 스펙

기준: aws-cli 1.31.13, botocore 1.33.13, s3transfer 0.8.2. M0에서 `docs/compat-spec.md`로 옮긴다.

### A.1 플래그 타입

| 플래그 | 타입 | 선택지 / 기본값 |
|---|---|---|
| `--include`, `--exclude` | 값 1개, 반복 가능, 한 목록에 순서대로 쌓임 | |
| `--acl` | 문자열 | private, public-read, public-read-write, authenticated-read, aws-exec-read, bucket-owner-read, bucket-owner-full-control, log-delivery-write |
| `--follow-symlinks` / `--no-follow-symlinks` | 불리언 쌍 | 기본 따라감, 마지막에 준 것이 이김 |
| `--sse` | 값 0~1개 | AES256(값 없을 때), aws:kms |
| `--sse-c`, `--sse-c-copy-source` | 값 0~1개 | AES256 |
| `--request-payer` | 값 0~1개 | requester |
| `--storage-class` | 문자열 | STANDARD, REDUCED_REDUNDANCY, STANDARD_IA, ONEZONE_IA, INTELLIGENT_TIERING, GLACIER, DEEP_ARCHIVE, GLACIER_IR |
| `--grants` | 값 1개 이상 | `권한=대상`, 권한은 read, readacl, writeacl, full |
| `--metadata` | 맵 | `k=v,k2=v2` 또는 `{`로 시작하는 JSON |
| `--metadata-directive` | 문자열 | COPY, REPLACE |
| `--page-size`, `--expires-in` | 정수 | `--expires-in` 기본 3600 |
| `--expected-size`, `--expires` | 문자열 | `--expires`는 그대로 전달 |
| `--cli-read-timeout`, `--cli-connect-timeout` | 정수 | 기본 60, 0은 무제한 |
| `--output` | 문자열 | json, text, table |
| `--color` | 문자열 | on, off, auto |
| 나머지 | 불리언 또는 문자열 | |

### A.2 경로 규칙

- `s3://`로 시작하면 S3, 아니면 로컬. 허용 조합: cp/mv/sync는 로컬→S3, S3→로컬, S3→S3. rm/mb/rb는 S3만. 그 밖은 `usage: aws s3 <cmd> <USAGE>\nError: Invalid argument type`, 종료 코드 255.
- `ls`, `presign`, `website`는 `s3://` 없이도 받는다.
- `s3://bucket`은 `s3://bucket/`으로 본다. `sync`, `mb`, `rb`는 항상 디렉터리 동작.
- 대상 경로: 디렉터리 동작이면 원본 루트 기준 상대 경로를 붙인다. 아니면 대상이 `/`로 끝나거나 기존 디렉터리일 때만 원본 파일명을 붙인다.
  - `cp f s3://b/p/` → `p/f`, `cp f s3://b/p` → `p`, `cp s3://b/k .` → `./k`, `cp s3://b/k newdir/` → `newdir/k`
- 재귀가 아닌 S3 원본은 `HeadObject`로 확인한다. 삭제는 생략한다. 404면 `Key "<key>" does not exist`.
- `s3://b/`를 재귀 없이 주면 아무것도 하지 않고 종료 코드 0.
- `mv`에서 원본과 대상이 같으면 `Cannot mv a file onto itself: '%s' - '%s'`, 255.
- 로컬 원본이 없으면 `The user-provided path %s does not exist.`, 255.
- S3→로컬 디렉터리 동작은 `--dryrun`에서도 대상 디렉터리를 만든다.
- `-`는 재귀가 아닌 `cp`에서만 허용: `Streaming currently is only compatible with non-recursive cp commands`, 255. 스트리밍이면 `--only-show-errors`가 강제된다.
- SSE-C 검증 문구(원문 오타 "It" 포함): `It --sse-c is specified, --sse-c-key must be specified as well.` 등, 255.
- `rb`에 키가 있으면 `Please specify a valid bucket name only. E.g. s3://%s`, 255. `--force`는 `rm --recursive`를 먼저 돌리고 0이 아니면 `remove_bucket failed: Unable to delete all objects in the bucket, bucket will not be deleted.`, 255.
- `mb`는 리전이 `us-east-1`이 아니면 `LocationConstraint`로 보낸다.
- `ls`: 버킷이 없으면 `ListBuckets`. `--recursive`면 구분자 없이 전체 키, 아니면 구분자 `/`로 마지막 구성 요소만 표시.
- `--source-region`은 S3→S3에서 원본 클라이언트를 따로 만든다. 이 클라이언트에는 `--endpoint-url`이 적용되지 않고 환경변수·설정의 엔드포인트만 적용된다.

### A.3 파싱 규칙

- 3단계: 전역 파서가 argv 전체에서 전역 옵션을 먼저 떼고, `s3`, 하위 명령 순으로 나머지를 넘긴다.
- 긴 플래그는 유일한 접두어로 줄여 쓸 수 있다(`--recur`, `--exc`). 전역 파서가 먼저 돌기 때문에 전역 옵션과 유일하게 맞는 접두어는 전역으로 잡힌다(`--re x` → `--region x`).
- 남는 토큰은 `"\nUnknown options: a,b\n"`을 표준 오류에 쓰고 255.
- argparse 오류(위치 인자 누락, 잘못된 선택지, 값 누락)는 usage와 `aws: error: <msg>`를 표준 오류에 쓰고 2.
- 위치 인자 2개는 붙어 있어야 한다. `cp a b --recursive`와 `cp --recursive a b`는 되고 `cp a --recursive b`는 `the following arguments are required: paths`, 2.
- 값 0~1개 플래그(`--sse` 등)는 뒤의 위치 인자를 삼킨다. `--grants`는 다음 `-` 토큰 전까지 모두 삼킨다.
- paramfile: `file://`(텍스트), `fileb://`(바이트), `http(s)://`(`cli_follow_urlparam`이 true일 때 GET). 문자열 하나짜리 인자에만 적용된다. `--website-redirect`, cp/mv/sync의 경로 2개, 필터, 전역 옵션에는 적용되지 않는다. 실패 시 `Error parsing parameter '<name>': Unable to load paramfile <path>: <err>`, 255.
- `--endpoint-url`에 스킴이 없으면 `Bad value for --endpoint-url "%s": scheme is missing. Must be of the form http://<hostname>/ or https://<hostname>/`, 255.

### A.4 필터

- Python `fnmatch`: `*`는 `/`도 넘어 일치, `?`, `[seq]`, `[!seq]`. POSIX에서 대소문자 구분.
- 패턴은 루트에 붙여 전체 경로와 비교한다. 로컬 루트는 원본의 절대 경로(디렉터리 동작이 아니면 그 상위), S3 루트는 `bucket/key`.
- 같은 패턴을 대상 루트에도 붙여 대상 목록에 적용한다. 그래서 제외된 파일은 `--delete`로 지워지지 않는다.
- 파일은 포함 상태로 시작하고, 규칙을 순서대로 보며 일치할 때마다 상태를 덮어쓴다.

### A.5 요청 파라미터

- Put/Copy/CreateMultipartUpload: ACL, StorageClass, WebsiteRedirectLocation, ContentType, CacheControl, ContentDisposition, ContentEncoding, ContentLanguage, Expires, Grant*, Metadata, ServerSideEncryption, SSEKMSKeyId, SSECustomer*.
- Copy/UploadPartCopy: CopySourceSSECustomer*. Get: SSE-C, RequestPayer. Delete: RequestPayer. RequestPayer는 Head·List에도 붙는다.
- Copy에서 `--metadata`를 주고 지시자를 안 주면 `MetadataDirective=REPLACE`가 된다.
- MIME: `--content-type`이 없고 추측이 켜져 있으면 업로드는 로컬 경로로, 복사는 원본 키로 추측한다. 스트림은 추측하지 않는다.
- 5TiB 초과: `warning: File <rel> exceeds s3 upload limit of 5.0 TiB.` 경고 후 시도.

### A.6 sync 판정

- 두 목록을 비교 키(상대 경로, `/` 구분) 순으로 한 번에 병합한다.
  - 양쪽에 있음: 아래 전략으로 판정.
  - 원본에만 있음: 전송.
  - 대상에만 있음: 기본은 무시, `--delete`면 삭제.
- 기본 전략: 크기가 다르면 전송. 같으면 시간으로 판정.
  - 업로드·복사: 원본이 대상보다 **엄격히 새로우면** 전송.
  - 다운로드: 로컬이 S3보다 **엄격히 새로우면** 전송.
- `--size-only`: 크기가 다를 때만 전송.
- `--exact-timestamps`: 다운로드에서 시간이 정확히 같지 않으면 전송. 업로드·복사는 기본 규칙. `--size-only`와 함께 주면 `--exact-timestamps`가 이긴다.
- 로컬 시간은 `stat`(링크를 따라감)의 mtime, 마이크로초 유지. S3 시간은 `LastModified`.
- 대상 목록에서는 크기 0이고 `/`로 끝나는 키를 건너뛴다. 삭제 작업(`rm --recursive`)에서는 포함한다.
- S3 대상 삭제는 원본 클라이언트로 수행한다.

### A.7 경고와 건너뛰기

모두 표준 오류, 경고가 하나라도 있으면 종료 코드 2(실패가 없을 때).

- `warning: Skipping file <path>. File does not exist.` (깨진 링크 포함)
- `warning: Skipping file <path>. File is character special device, block special device, FIFO, or socket.`
- `warning: Skipping file <path>. File/Directory is not readable.`
- `warning: File has an invalid timestamp. Passing epoch time as timestamp.` (건너뛰지 않음)
- `--no-follow-symlinks`면 링크를 조용히 건너뛴다.
- 글레이셔: `StorageClass`가 GLACIER 또는 DEEP_ARCHIVE이고 복원 완료 표시가 없으면 다운로드·복사를 건너뛴다. 목록 조회 결과에는 복원 정보가 없어 재귀·sync에서는 항상 건너뛴다. `--force-glacier-transfer`면 전송, `--ignore-glacier-warnings`면 조용히 건너뛴다.
  - 문구: `warning: Skipping file s3://<bucket/key>. Object is of storage class GLACIER. Unable to perform <op> operations on GLACIER objects. You must restore the object to be able to perform the operation. See aws s3 <op> help for additional parameter options to ignore or force these transfers.`
- 다운로드 대상이 상위 디렉터리를 가리키면 `warning: Skipping file <key>. File references a parent directory.`
- mtime 설정 실패: `warning: Skipping file <file>. Successfully Downloaded <file> but was unable to update the last modified time. <err>`

### A.8 출력 형식

- 출력기 선택 순서: `--quiet`(오류·경고 포함 아무것도 안 냄) → `--only-show-errors` 또는 스트림(진행률·성공 줄 없음, 실패·경고·dryrun 줄은 냄) → `--no-progress` → 기본.
- 성공: `{종류}: {원본} to {대상}`, 대상이 없으면 `{종류}: {원본}`. dryrun은 앞에 `(dryrun) `.
- 실패: `{종류} failed: {원본} to {대상} {오류}`. 치명적 오류: `fatal error: {오류}`.
- 종류는 upload, download, copy, delete, 그리고 mv에서는 move.
- 로컬 경로는 현재 디렉터리 기준 상대 경로로 표시한다(`./a.txt`, `sub/a.txt`, `../x/a.txt`). S3는 `s3://bucket/key`, 스트림은 `-`.
- 진행률(표준 출력, TTY 여부를 보지 않음):
  - `Completed {완료}/{전체} ({속도}/s) with {n} file(s) remaining`
  - 바이트가 없는 작업: `Completed {파일 수} file(s) with {n} file(s) remaining`
  - 전체 수를 모르는 동안 `{전체}`와 `{n}` 앞에 `~`, 끝에 ` (calculating...)`.
  - 이전 진행률 길이만큼 공백으로 채우고 `\r`로 끝난다. 결과 줄도 같은 길이로 채운 뒤 `\n`.
- 크기 표기: `1 Byte`, 1024 미만은 `N Bytes`, 그 위는 소수 한 자리 `KiB`…`EiB`. 1024 → `1.0 KiB`, 1048575 → `1.0 MiB`.
- `ls`:
  - 버킷: `YYYY-MM-DD HH:MM:SS 이름` (로컬 시간)
  - 접두어: 공백 27개 + `PRE` + 공백 + `이름/`
  - 객체: `날짜(19자) 크기(오른쪽 정렬 10자) 이름`
  - `--summarize`: `\nTotal Objects: N\n` + `   Total Size: S\n`
- `presign`: URL과 줄바꿈. `mb`: `make_bucket: <bucket>`. `rb`: `remove_bucket: <bucket>`. 실패는 `make_bucket failed: <path> <오류>` 형식으로 표준 오류, 종료 코드 1. `website`: 출력 없음.

### A.9 종료 코드

| 코드 | 조건 |
|---|---|
| 0 | 성공. 버킷 루트나 전체 버킷 `ls` 포함 |
| 1 | 전송 실패가 하나라도 있음. 목록 조회 중 오류(HeadObject 404, NoSuchBucket, AccessDenied). 키가 있는 `ls`에서 첫 페이지에 객체도 접두어도 없음. `mb`/`rb` API 실패 |
| 2 | 실패 없이 경고만 있음. argparse 인자 오류 |
| 255 | 그 밖의 예외: Unknown options, 잘못된 경로 조합, 없는 로컬 원본, 스트리밍 오용, SSE-C 검증, 같은 경로 `mv`, `ls`/`website`/`presign`의 API 오류, 리전·자격 증명·프로필 오류, 잘못된 `s3` 설정값 |

### A.10 환경변수와 설정

- 프로필: `AWS_DEFAULT_PROFILE`이 `AWS_PROFILE`보다 먼저, `--profile`이 둘보다 먼저.
- 리전: `AWS_DEFAULT_REGION`. 이 버전은 `AWS_REGION`을 읽지 않는다.
- `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`, `AWS_CA_BUNDLE`, `AWS_MAX_ATTEMPTS`, `AWS_RETRY_MODE`.
- 엔드포인트: `AWS_ENDPOINT_URL_S3` → `AWS_ENDPOINT_URL` → `services` 섹션 → 프로필 `endpoint_url`. `AWS_IGNORE_CONFIGURED_ENDPOINT_URLS`로 끌 수 있다.
- `s3 =` 블록의 크기 값은 정수 또는 `KB/MB/GB/TB/KiB/MiB/GiB/TiB`(모두 1024 기반). `max_bandwidth`는 `B/s`로 끝나야 한다. 오류 문구: `Invalid size value: …`, `Value for %s must be a positive integer: %s`.
