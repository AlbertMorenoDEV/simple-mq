Simple MQ
=========

Publish a message
-----------------
```
curl --header "Content-Type: application/json" \
  --request POST \
  --data '{"id":"1","type":"test","data":"hello"}' \
  http://localhost:8001/
```


Get a message
-----------------
```
curl http://localhost:8002/
```


Tools
-----
* End2End package tool: https://github.com/h2non/baloo


Changelog
---------
### v0.1
* HTTP Publisher with a single message
* HTTP Subscriber


ToDo
----
* Persist queue items
* Multiple subscribers delivery
* Multiple queue
* Socket publisher
* Socket subscriber