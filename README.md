Simple MQ
=========

Publish a message
-----------------

`curl -X POST -d "{\"id\": \"1\", \"type\": \"test\", \"data\": \"hello\"}" http://localhost:9000/`

Changelog
---------

### v0.1
* HTTP Publisher with a single message
* HTTP Subscriber