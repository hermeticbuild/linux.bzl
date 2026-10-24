"""Decode the existing filename transport across chunk boundaries."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//rules:runtime.bzl", "read_payload")
load("//rules:schedule.bzl", "decode_schedule", "encode_schedule", "write_payload", "write_schedule")

def _payload_test(ctx):
    env = unittest.begin(ctx)

    # The escaped backslash begins at the last character of the first chunk.
    # The reader must join chunks before interpreting either filename or JSON
    # escapes, and sort by the index rather than the directory traversal order.
    encoded = '{"value":"' + "a" * 169 + "%5Cu03bb" + "a" * 20000 + '"}'
    children = []
    for start in range(0, len(encoded), 180):
        number = ("00000000" + str(start // 180))[-8:]
        children.append(struct(basename = number + "=" + encoded[start:start + 180]))
    asserts.true(env, len(children) > 100)
    asserts.true(env, children[0].basename.endswith("%"))
    asserts.equals(env, {"value": "a" * 169 + "λ" + "a" * 20000}, read_payload(struct(children = children[::-1] + [struct(basename = "report.json")])))
    asserts.equals(env, [], read_payload(struct(children = [struct(basename = "00000000=[]")])))

    # Inspect the production writer's filenames and report, including a split
    # slash escape and literal text that must not be recursively unescaped.
    files, writes = [], []

    def declare_file(name, directory):
        file = struct(path = directory.path + "/" + name, basename = name.rsplit("/", 1)[-1])
        files.append(file)
        return file

    def write(output, content):
        writes.append((output, content))

    value = {"value": "a" * 169 + "/\\%2F%5C%25\n" + "a" * 20000}
    write_payload(struct(declare_file = declare_file, write = write), struct(path = "payload"), value)
    expected = '{"value":"' + "a" * 169 + "%2F%5C%5C%252F%255C%2525%5Cn" + "a" * 20000 + '"}'
    chunks = [file for file in files if "=" in file.basename]
    asserts.equals(env, expected, "".join([file.basename.split("=", 1)[1] for file in chunks]))
    asserts.equals(env, "payload/00/00/00/00000000=" + expected[:180], chunks[0].path)
    asserts.equals(env, "payload/00/00/01/00000100=" + expected[18000:18180], chunks[100].path)
    asserts.equals(env, ("payload/report.json", json.encode(value)), (writes[0][0].path, writes[0][1]))
    asserts.equals(env, [(file, "") for file in chunks], writes[1:])
    asserts.equals(env, value, read_payload(struct(children = files[::-1])))

    # Printable ASCII is retained verbatim except the three reserved bytes;
    # JSON still owns escaping of control characters.
    files.clear()
    writes.clear()
    value = [" !\"#$%&'()*+,-./0123456789:;<=>?@ABCDEFGHIJKLMNOPQRSTUVWXYZ[\\]^_`abcdefghijklmnopqrstuvwxyz{|}~", "\n\r\t", [], {}]
    write_payload(struct(declare_file = declare_file, write = write), struct(path = "payload"), value)
    asserts.equals(env, value, read_payload(struct(children = files[::-1])))

    # Unequal input lists share strings without losing order or duplicates.
    jobs = [
        dict(inputs = ["source.c", "generated.h", "source.c"], outputs = ["object.o"], scan = {"generated": {"header.h": "generated.h"}}, compiler = {"phase": "single"}),
        dict(inputs = ["generated.h", "object.o"], outputs = ["archive.a"], scan = None, compiler = {}),
        dict(inputs = [], outputs = ["constant"], scan = None, compiler = {}),
    ]
    compact = encode_schedule(jobs)
    asserts.equals(env, ["source.c", "generated.h", "object.o"], compact["paths"])
    asserts.equals(env, [[0, 1, 0], [1, 2], []], [job["inputs"] for job in compact["jobs"]])
    asserts.equals(env, jobs, decode_schedule(compact))
    files.clear()
    writes.clear()
    write_schedule(struct(declare_file = declare_file, write = write), struct(path = "payload"), jobs)
    asserts.equals(env, ("payload/report.json", json.encode(jobs)), (writes[0][0].path, writes[0][1]))
    asserts.equals(env, compact, read_payload(struct(children = files[::-1])))
    asserts.equals(env, jobs, decode_schedule(read_payload(struct(children = files))))

    # Large payloads still write every declared filename chunk.
    files.clear()
    writes.clear()
    write_payload(struct(declare_file = declare_file, write = write), struct(path = "payload"), "a" * 100000)
    asserts.equals(env, 556, len(writes) - 1)
    asserts.equals(env, [(file, "") for file in files[1:]], writes[1:])
    return unittest.end(env)

payload_test = unittest.make(_payload_test)
