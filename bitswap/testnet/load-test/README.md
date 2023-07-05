# Experiment

This code serves as an experiment to see the influence of request load on block retrieval time.

- bc.go -- block creation  
    - generates blocks which the server stores and the client fetches  
- sbst.go -- server Bitswap test  
    - starts a Bitswap instance and provides blocks
    - expects a folder "blk" with 1k blocks  
    - adjust IP, port, Bitswap protocol as desired  
- cbst.go -- client Bitswap test  
    - start a Bitswap instance and libp2p hosts  
    - the libp2p hosts send WANT-HAVE requests to the server  
    - the Bitswap instance fetches blocks from the server  
        - fetches a single example block created by the blocksutil.BlockGenerator  
        - waits for the libp2p hosts to finish sending their requests  
        - fetches the set number of blocks from the "blk" folder  
        - writes the retrieval time of the blocks in the result file  
    - adjust IP, port, Bitswap protocol as desired/required  
