Simple MQ
=========

Publish a message
-----------------

`curl -X POST -d "{\"id\": \"1\", \"type\": \"test\", \"data\": \"hello\"}" http://localhost:8001/`


Get a message
-----------------

`curl http://localhost:8002/`

Changelog
---------

### v0.1
* HTTP Publisher with a single message
* HTTP Subscriber