from flask import Flask, request, jsonify
from pymongo import MongoClient
import requests
from config import MONGO_URI, DATABASE_NAME, COLLECTION_NAME

app = Flask(__name__)

# Setup MongoDB client
client = MongoClient(MONGO_URI)
db = client[DATABASE_NAME]
routes_collection = db[COLLECTION_NAME]

# Rute untuk menangani permintaan ke root URL (/)
@app.route('/', methods=['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
def root_gateway():
    return gateway('')

@app.route('/<path:path>', methods=['GET', 'POST', 'PUT', 'PATCH', 'DELETE'])
def gateway(path):
    host_url = request.headers.get("host")
    route = routes_collection.find_one({'host_url': host_url})
    if not route:
        return jsonify({'error': 'Route not found'}), 404
    
    target_url = route['target_url'].rstrip('/')
    full_target_url = f"http://{target_url}/{path}" if path else target_url

    print("full_target_url:", full_target_url)
    
    method = request.method
    headers = {key: value for key, value in request.headers if key.lower() != 'host'}
    
    try:
        if method == 'GET':
            response = requests.get(full_target_url, headers=headers, params=request.args)
        elif method == 'POST':
            if request.files:
                files = {key: (file.filename, file.stream, file.mimetype) for key, file in request.files.items()}
                response = requests.post(full_target_url, headers=headers, data=request.form, files=files)
            else:
                response = requests.post(full_target_url, headers=headers, json=request.json)
        elif method == 'PUT':
            if request.files:
                files = {key: (file.filename, file.stream, file.mimetype) for key, file in request.files.items()}
                response = requests.put(full_target_url, headers=headers, data=request.form, files=files)
            else:
                response = requests.put(full_target_url, headers=headers, json=request.json)
        elif method == 'PATCH':
            if request.files:
                files = {key: (file.filename, file.stream, file.mimetype) for key, file in request.files.items()}
                response = requests.patch(full_target_url, headers=headers, data=request.form, files=files)
            else:
                response = requests.patch(full_target_url, headers=headers, json=request.json)
        elif method == 'DELETE':
            response = requests.delete(full_target_url, headers=headers)
        else:
            return jsonify({'error': 'Method not allowed'}), 405
        
        return (response.content, response.status_code, response.headers.items())
    except requests.exceptions.RequestException as e:
        return jsonify({'error': str(e)}), 500

if __name__ == '__main__':
    app.run(debug=True, port=8880)
