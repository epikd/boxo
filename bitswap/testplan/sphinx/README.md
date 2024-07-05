# Test of the Sphinx modification

Very simple [Testground](https://github.com/testground/testground) testplan to test the Sphinx functionality.  
The test starts bitswap nodes (at least 3).
The generated public keys are exchanged and forwarded to bitswap.
Afterwards, all nodes establish a connection with each other.
One node is a Server (provider), one node is the client (requester), the rest are passive.
"count" blocks are added to the Server and the CIDs are shared with the Clients.
Then the Client attempts to download the blocks using the modified code.
For the Sphinx path, bitswap sends WANT-HAVE request over other neighbors to the neighbors.
The logging is set to "info", showing the changes in the messages.

## Running the test

To run the test [install](https://docs.testground.ai/getting-started) Testground.  

Import the testplan:

```
testground plan import --from [path-to-testplan]
```

Run the test with the desired parameters e.g.:

```
testground run single --plan=sphinx --testcase=speed-test --builder="docker:go" --runner="local:docker" --instances=3 --tp size=512kiB --tp count=5 --tp hops=2
```

Since the execution time might take a while, it can be necessary to increase the task timeout time in "~/.config/testground/env.toml"  

```
[daemon.scheduler]
task_timeout_min = 30
```